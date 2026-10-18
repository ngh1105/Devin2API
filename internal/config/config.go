package config

import "os"

// Config holds the application configuration.
type Config struct {
	Listen  string
	DataDir string
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
	return cfg
}
