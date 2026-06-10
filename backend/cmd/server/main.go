package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/api/routes"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/auth"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/config"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/database"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/plugins"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/rpc"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/services/notifications"
	"github.com/ValwareIRC/unrealircd-webpanel-2/internal/services/scheduler"
	"github.com/gin-gonic/gin"
)

func main() {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// config.json holds the argon2 pepper, JWT secret and the RPC
	// server list -- it MUST live on the persistent volume or every
	// container restart regenerates the pepper and every existing
	// admin password hash becomes unverifiable. /app/data is the
	// declared volume; if a legacy config.json still sits in the
	// working directory, migrate it over so first-restart accounts
	// keep working. WEBPANEL_CONFIG_PATH overrides this for tests.
	configPath := envOr("WEBPANEL_CONFIG_PATH", "data/config.json")
	migrateLegacyConfig(configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("Warning: Could not load config file: %v", err)
	}

	// Generate secrets if not set
	if cfg.Auth.JWTSecret == "" {
		jwtSecret, pepper, encKey, err := auth.GenerateSecrets()
		if err != nil {
			log.Fatalf("Failed to generate secrets: %v", err)
		}
		cfg.Auth.JWTSecret = jwtSecret
		cfg.Auth.PasswordPepper = pepper
		cfg.Auth.EncryptionKey = encKey

		if err := config.Save(configPath); err != nil {
			log.Printf("Warning: Could not save config: %v", err)
		}
	}

	// Initialize database
	if err := database.Initialize(&cfg.Database); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Initialize plugin system
	initializePlugins()

	// Check if admin user needs to be created
	checkAndCreateAdminUser(cfg)

	// Seed an RPC server from environment variables (Coolify / docker-compose).
	// Idempotent: an existing entry with the same name or (host, port, user)
	// triple is left alone, so editing it in the UI sticks across restarts.
	seedRPCServerFromEnv(cfg)

	// Connect to RPC servers
	connectToRPCServers(cfg)

	// Initialize notification service (registers webhook hooks)
	notifications.Initialize()

	// Initialize scheduler service (handles cron jobs, scheduled commands, digests)
	sched := scheduler.Initialize()
	defer sched.Stop()

	// Setup graceful shutdown
	setupGracefulShutdown(sched)

	// Setup Gin
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.Default()

	// Serve static files for frontend (in production)
	r.Static("/assets", "./frontend/assets")
	r.StaticFile("/", "./frontend/index.html")
	r.StaticFile("/favicon.ico", "./frontend/favicon.ico")
	r.StaticFile("/favicon.svg", "./frontend/favicon.svg")
	r.StaticFile("/unreal.png", "./frontend/unreal.png")
	r.NoRoute(func(c *gin.Context) {
		c.File("./frontend/index.html")
	})

	// Setup API routes
	routes.SetupRoutes(r)

	// Start server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Printf("Starting server on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func checkAndCreateAdminUser(cfg *config.Config) {
	db := database.Get()

	// Check if any users exist
	var count int64
	if err := db.Table("users").Count(&count).Error; err != nil {
		log.Printf("Warning: Could not count users: %v", err)
		return
	}

	if count == 0 {
		// Create default admin user. Allow Coolify / docker-compose to
		// override the credentials via env so first boot doesn't need
		// the admin/admin123 round-trip + manual password change.
		log.Println("No users found. Creating default admin user...")

		adminUser := envOr("WEBPANEL_ADMIN_USER", "admin")
		adminEmail := envOr("WEBPANEL_ADMIN_EMAIL", "admin@localhost")
		adminPass := envOr("WEBPANEL_ADMIN_PASSWORD", "admin123")

		// Get Super-Admin role
		var roleID uint = 1 // Default to first role (Super-Admin)

		user, err := auth.CreateUser(adminUser, adminEmail, adminPass, "Admin", "User", roleID)
		if err != nil {
			log.Printf("Warning: Could not create default admin user: %v", err)
		} else {
			passwordHint := "admin123"
			if adminPass != "admin123" {
				passwordHint = "(from WEBPANEL_ADMIN_PASSWORD env)"
			}
			fmt.Printf("Created default admin user: %s (password: %s)\n", user.Username, passwordHint)
			if passwordHint == "admin123" {
				fmt.Println("Please change this password immediately!")
			}
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// migrateLegacyConfig moves a config.json left in the working directory
// (pre-volume-aware builds wrote it there, so it'd vanish on restart and
// take the argon2 pepper with it) into the volume-backed location. If
// dst already exists we leave both files alone -- the volume copy is
// authoritative and the working-dir copy is stale.
func migrateLegacyConfig(dst string) {
	const legacy = "config.json"
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	if err := os.MkdirAll(filepathDir(dst), 0755); err != nil {
		log.Printf("config-migrate: mkdir %s: %v", filepathDir(dst), err)
		return
	}
	in, err := os.ReadFile(legacy)
	if err != nil {
		log.Printf("config-migrate: read %s: %v", legacy, err)
		return
	}
	if err := os.WriteFile(dst, in, 0600); err != nil {
		log.Printf("config-migrate: write %s: %v", dst, err)
		return
	}
	log.Printf("config-migrate: copied %s -> %s (pepper + JWT secret preserved across restart)", legacy, dst)
}

// filepathDir avoids pulling in path/filepath just for one helper.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// seedRPCServerFromEnv adds an RPCServer entry to cfg if UNREAL_RPC_URL,
// UNREAL_RPC_USERNAME and UNREAL_RPC_PASSWORD are all set in the
// environment and no matching server already exists. URL accepts the
// ws://host[:port]/, wss://host[:port]/, http(s)://, or bare host[:port]
// forms; missing port defaults to 8080. If no default RPC server is
// configured yet, the seeded one becomes the default.
func seedRPCServerFromEnv(cfg *config.Config) {
	rawURL := os.Getenv("UNREAL_RPC_URL")
	user := os.Getenv("UNREAL_RPC_USERNAME")
	pass := os.Getenv("UNREAL_RPC_PASSWORD")
	if rawURL == "" || user == "" || pass == "" {
		return
	}
	name := os.Getenv("UNREAL_RPC_NAME")
	if name == "" {
		name = "default"
	}

	host, port, tls, err := parseRPCEndpoint(rawURL)
	if err != nil {
		log.Printf("env-seed: ignoring UNREAL_RPC_URL=%q: %v", rawURL, err)
		return
	}

	for _, s := range cfg.RPC {
		if s.Name == name {
			return
		}
		if s.Host == host && s.Port == port && s.User == user {
			return
		}
	}

	hasDefault := false
	for _, s := range cfg.RPC {
		if s.IsDefault {
			hasDefault = true
			break
		}
	}

	cfg.RPC = append(cfg.RPC, config.RPCServer{
		Name:          name,
		Host:          host,
		Port:          port,
		User:          user,
		Password:      pass,
		TLSVerifyCert: tls,
		IsDefault:     !hasDefault,
	})

	if err := config.Save("config.json"); err != nil {
		log.Printf("env-seed: failed to persist seeded RPC server %q: %v", name, err)
		return
	}

	log.Printf("env-seed: added RPC server %q -> %s:%d (user=%s tls=%v default=%v)",
		name, host, port, user, tls, !hasDefault)
}

func parseRPCEndpoint(raw string) (host string, port int, tls bool, err error) {
	port = 8080
	parsed, parseErr := url.Parse(raw)
	if parseErr == nil && parsed.Host != "" {
		host = parsed.Hostname()
		switch parsed.Scheme {
		case "wss", "https":
			tls = true
		}
		if p := parsed.Port(); p != "" {
			n, convErr := strconv.Atoi(p)
			if convErr != nil {
				return "", 0, false, fmt.Errorf("non-numeric port %q", p)
			}
			port = n
		}
		return host, port, tls, nil
	}
	// Bare host[:port]
	host = raw
	if colon := indexOf(raw, ':'); colon != -1 {
		host = raw[:colon]
		n, convErr := strconv.Atoi(raw[colon+1:])
		if convErr != nil {
			return "", 0, false, fmt.Errorf("non-numeric port %q", raw[colon+1:])
		}
		port = n
	}
	if host == "" {
		return "", 0, false, fmt.Errorf("empty host")
	}
	return host, port, false, nil
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func connectToRPCServers(cfg *config.Config) {
	manager := rpc.GetManager()

	for _, server := range cfg.RPC {
		log.Printf("Connecting to RPC server: %s (%s:%d)", server.Name, server.Host, server.Port)

		_, err := manager.Connect(&server, "webpanel")
		if err != nil {
			log.Printf("Warning: Could not connect to %s: %v", server.Name, err)
			continue
		}

		log.Printf("Connected to RPC server: %s", server.Name)
	}
}

func setupGracefulShutdown(sched *scheduler.Scheduler) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		log.Println("Shutting down gracefully...")
		sched.Stop()
		shutdownPlugins()
		os.Exit(0)
	}()
}

func initializePlugins() {
	manager := plugins.GetManager()

	// Determine plugin directory
	pluginDir := "plugins"
	if _, err := os.Stat("/home/valerie/uwp-plugins/plugins"); err == nil {
		pluginDir = "/home/valerie/uwp-plugins/plugins"
	} else if _, err := os.Stat("internal/plugins"); err == nil {
		pluginDir = "internal/plugins"
	}
	manager.SetPluginDir(pluginDir)

	// Create plugins directory if it doesn't exist
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		log.Printf("Warning: Could not create plugins directory: %v", err)
	}

	// Load installed plugins from database
	db := database.Get()
	var installedPlugins []database.InstalledPlugin
	if err := db.Where("enabled = ?", true).Find(&installedPlugins).Error; err != nil {
		log.Printf("Warning: Could not load installed plugins from database: %v", err)
		return
	}

	for _, p := range installedPlugins {
		log.Printf("Loading plugin: %s", p.ID)
		if err := manager.LoadPlugin(p.ID); err != nil {
			// Not an error - most plugins are "virtual" without .so files
			log.Printf("Plugin %s registered (no runtime component): %v", p.ID, err)
		} else {
			log.Printf("Loaded plugin: %s v%s", p.Name, p.Version)
		}
	}

	log.Printf("Plugin system initialized. %d plugins loaded.", len(manager.ListPlugins()))
}

func shutdownPlugins() {
	manager := plugins.GetManager()
	for _, p := range manager.ListPlugins() {
		log.Printf("Shutting down plugin: %s", p.Handle)
		if err := manager.UnloadPlugin(p.Handle); err != nil {
			log.Printf("Warning: Error shutting down plugin %s: %v", p.Handle, err)
		}
	}
}
