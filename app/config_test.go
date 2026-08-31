package app

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func setRouterEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LOOMUX_ROUTER_PRIMARY_BASE_URL", "https://example.invalid/v1")
	t.Setenv("LOOMUX_ROUTER_PRIMARY_API_KEY", "test-key")
	t.Setenv("LOOMUX_ROUTER_PRIMARY_MODEL", "test-model")
}

func TestLoadConfig_Defaults(t *testing.T) {
	setRouterEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DBPath != defaultDBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, defaultDBPath)
	}
	if cfg.MarkerDir != "" {
		t.Errorf("MarkerDir = %q, want empty", cfg.MarkerDir)
	}
	if cfg.MasterKey != nil {
		t.Errorf("MasterKey = %v, want nil", cfg.MasterKey)
	}
	if cfg.ReapIdleThreshold != defaultReapIdleThreshold {
		t.Errorf("ReapIdleThreshold = %v, want %v", cfg.ReapIdleThreshold, defaultReapIdleThreshold)
	}
	if cfg.ReapInterval != defaultReapInterval {
		t.Errorf("ReapInterval = %v, want %v", cfg.ReapInterval, defaultReapInterval)
	}
}

func TestLoadConfig_CustomReapTiming(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envReapIdleThreshold, "48h")
	t.Setenv(envReapInterval, "30m")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReapIdleThreshold != 48*time.Hour {
		t.Errorf("ReapIdleThreshold = %v, want 48h", cfg.ReapIdleThreshold)
	}
	if cfg.ReapInterval != 30*time.Minute {
		t.Errorf("ReapInterval = %v, want 30m", cfg.ReapInterval)
	}
}

func TestLoadConfig_InvalidReapTiming(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envReapIdleThreshold, "not-a-duration")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig: want error for a malformed reap idle threshold")
	}
}

func TestLoadConfig_CustomDBPath(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envDBPath, "/tmp/custom.db")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/tmp/custom.db")
	}
}

func TestLoadConfig_MasterKey(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envMasterKey, base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.MasterKey) != 32 {
		t.Errorf("len(MasterKey) = %d, want 32", len(cfg.MasterKey))
	}
}

func TestLoadConfig_InvalidMasterKey(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envMasterKey, "not-valid-base64!!")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig: want error for invalid master key, got nil")
	}
}

func TestLoadConfig_MissingRouterConfig(t *testing.T) {
	// deliberately not calling setRouterEnv
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "router config") {
		t.Fatalf("LoadConfig err = %v, want it to mention router config", err)
	}
}
