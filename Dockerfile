FROM node:22-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS backend
WORKDIR /src
RUN apk add --no-cache git
COPY backend/go.mod backend/go.sum ./backend/
RUN cd backend && go mod download
COPY backend/ ./backend/
RUN cd backend && CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o /out/webpanel ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S panel && adduser -S -G panel panel
WORKDIR /app
COPY --from=backend  /out/webpanel        ./webpanel
COPY --from=frontend /src/frontend/dist   ./frontend
COPY config.example.json                  ./config.example.json
RUN mkdir -p /app/data && chown -R panel:panel /app
USER panel
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["./webpanel"]
