package config

import (
	"encoding/json"
	"os"
	"sync"
)

// Config holds all application configuration
type Config struct {
	Server   ServerConfig   `json:"server"`
	Database DatabaseConfig `json:"database"`
	Auth     AuthConfig     `json:"auth"`
	RPC      []RPCServer    `json:"rpc_servers"`
	Plugins  []string       `json:"plugins"`
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// DatabaseConfig holds database configuration
type DatabaseConfig struct {
	Driver      string `json:"driver"` // sqlite or mysql
	DSN         string `json:"dsn"`
	TablePrefix string `json:"table_prefix"`
}

// AuthConfig holds authentication configuration
type AuthConfig struct {
	JWTSecret      string `json:"jwt_secret"`
	SessionTimeout int    `json:"session_timeout"` // seconds
	PasswordPepper string `json:"password_pepper"`
	EncryptionKey  string `json:"encryption_key"`
}

// RPCServer holds UnrealIRCd RPC server configuration
type RPCServer struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"rpc_user"`
	Password      string `json:"rpc_password"` // encrypted
	TLSVerifyCert bool   `json:"tls_verify_cert"`
	IsDefault     bool   `json:"is_default"`
}

var (
	cfg     *Config
	cfgPath string
	once    sync.Once
)

// Load loads the configuration from file and remembers the path so
// later Save calls don't need it threaded through every call site.
func Load(path string) (*Config, error) {
	var err error
	once.Do(func() {
		cfgPath = path
		cfg = &Config{
			Server: ServerConfig{
				Host: "0.0.0.0",
				Port: 8080,
			},
			Database: DatabaseConfig{
				Driver:      "sqlite",
				DSN:         "data/webpanel.db",
				TablePrefix: "unreal_",
			},
			Auth: AuthConfig{
				SessionTimeout: 3600,
			},
			Plugins: []string{},
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				err = readErr
			}
			return
		}

		if jsonErr := json.Unmarshal(data, cfg); jsonErr != nil {
			err = jsonErr
		}
	})

	return cfg, err
}

// Get returns the current configuration
func Get() *Config {
	if cfg == nil {
		cfg = &Config{}
	}
	return cfg
}

// Save persists the current configuration. If the caller passes the
// legacy bare "config.json" we redirect to the path Load was given,
// so existing call sites keep working even when the on-disk file
// moved to the persistent /app/data volume.
func Save(path string) error {
	if path == "config.json" && cfgPath != "" {
		path = cfgPath
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// GetDefaultRPCServer returns the default RPC server configuration
func (c *Config) GetDefaultRPCServer() *RPCServer {
	for i := range c.RPC {
		if c.RPC[i].IsDefault {
			return &c.RPC[i]
		}
	}
	if len(c.RPC) > 0 {
		return &c.RPC[0]
	}
	return nil
}

// GetRPCServer returns an RPC server by name
func (c *Config) GetRPCServer(name string) *RPCServer {
	for i := range c.RPC {
		if c.RPC[i].Name == name {
			return &c.RPC[i]
		}
	}
	return nil
}
