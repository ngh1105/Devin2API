package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the application configuration.
type Config struct {
	Listen  string
	DataDir string
	BaseURL string // optional: overrides auth client base URL (tests only)
	// AdminPassword, when set, protects the panel and admin API with a
	// login and allows using them from non-loopback hosts (e.g. the LAN).
	AdminPassword string
	// ResponsesKeep, ResponsesTTL and ResponsesMB bound the in-memory store
	// of /v1/responses histories kept for previous_response_id: how many,
	// for how long, and their total size. Zero means the server default.
	ResponsesKeep int
	ResponsesTTL  time.Duration
	ResponsesMB   int
}

// Load reads configuration from environment variables, falling back to defaults.
func Load() Config {
	cfg := Config{
		Listen:  "127.0.0.1:8080",
		DataDir: "./data",
	}
	if v := os.Getenv("DEVIN2API_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("DEVIN2API_DATA"); v != "" {
		cfg.DataDir = v
	}
	cfg.AdminPassword = os.Getenv("DEVIN2API_ADMIN_PASSWORD")
	cfg.ResponsesKeep = envInt("DEVIN2API_RESPONSES_KEEP")
	cfg.ResponsesMB = envInt("DEVIN2API_RESPONSES_MB")
	if v := os.Getenv("DEVIN2API_RESPONSES_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.ResponsesTTL = d
		}
	}
	return cfg
}

// envInt reads a positive integer variable; anything else is 0 (default).
func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
