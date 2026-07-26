// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"strings"
)

// Config holds the application configuration.
type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string
	// DBPath is the path to the SQLite database file.
	DBPath string
	// AdminPassword is used only on first-run seeding of the admin user.
	AdminPassword string
	// Secret signs yabs ingest URLs (HMAC). When empty, a random secret is
	// generated and persisted next to the DB on first boot.
	Secret string
	// BehindTLSProxy marks deployments behind an HTTPS-terminating reverse
	// proxy: session cookies go Secure and X-Forwarded-For is trusted.
	BehindTLSProxy bool
	// BaseURL overrides the external URL shown in the YABS ingest command
	// (e.g. "https://idlers.example.com"). Empty = derive from requests.
	BaseURL string
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	return Config{
		Addr:           getEnv("IDLER_ADDR", "127.0.0.1:8080"),
		DBPath:         getEnv("IDLER_DB", "./data/idlerthing.db"),
		AdminPassword:  os.Getenv("IDLER_ADMIN_PASSWORD"),
		Secret:         os.Getenv("IDLER_SECRET"),
		BehindTLSProxy: envBool("IDLER_BEHIND_TLS_PROXY"),
		BaseURL:        os.Getenv("IDLER_BASE_URL"),
	}
}

// envBool reports whether an env var is set to a truthy value.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
