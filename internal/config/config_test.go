package config_test

import (
	"os"
	"testing"

	"github.com/ngh1105/Devin2API/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	// Clear any env that might affect defaults.
	os.Clearenv()

	cfg := config.Load()

	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "127.0.0.1:8080")
	}
	if cfg.DataDir != "./data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "./data")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	os.Clearenv()
	os.Setenv("DEVIN2API_LISTEN", "0.0.0.0:9090")
	os.Setenv("DEVIN2API_DATA", "/tmp/devin2api")

	cfg := config.Load()

	if cfg.Listen != "0.0.0.0:9090" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "0.0.0.0:9090")
	}
	if cfg.DataDir != "/tmp/devin2api" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/tmp/devin2api")
	}
}
