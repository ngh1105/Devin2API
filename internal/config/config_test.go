package config_test

import (
	"os"
	"testing"
	"time"

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

func TestLoadResponsesLimits(t *testing.T) {
	os.Clearenv()
	os.Setenv("DEVIN2API_RESPONSES_KEEP", "50")
	os.Setenv("DEVIN2API_RESPONSES_TTL", "2h")
	os.Setenv("DEVIN2API_RESPONSES_MB", "128")
	cfg := config.Load()
	if cfg.ResponsesKeep != 50 || cfg.ResponsesTTL != 2*time.Hour || cfg.ResponsesMB != 128 {
		t.Errorf("responses limits = %d %v %d", cfg.ResponsesKeep, cfg.ResponsesTTL, cfg.ResponsesMB)
	}
	os.Setenv("DEVIN2API_RESPONSES_KEEP", "-1")
	os.Setenv("DEVIN2API_RESPONSES_TTL", "soon")
	cfg = config.Load()
	if cfg.ResponsesKeep != 0 || cfg.ResponsesTTL != 0 {
		t.Errorf("bad values not ignored: %d %v", cfg.ResponsesKeep, cfg.ResponsesTTL)
	}
}
