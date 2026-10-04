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
	if cfg.TargetProbeInterval != defaultTargetProbeInterval {
		t.Errorf("TargetProbeInterval = %v, want %v", cfg.TargetProbeInterval, defaultTargetProbeInterval)
	}
}

func TestLoadConfig_TargetProbeInterval(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envTargetProbeInterval, "90s")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TargetProbeInterval != 90*time.Second {
		t.Errorf("TargetProbeInterval = %v, want 90s", cfg.TargetProbeInterval)
	}
	for _, bad := range []string{"soon", "0s", "-1m"} {
		t.Setenv(envTargetProbeInterval, bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("LoadConfig with %s=%q: want an error", envTargetProbeInterval, bad)
		}
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

func TestLoadConfig_NotifyOffByDefault(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Notify.NtfyURL != "" {
		t.Errorf("Notify.NtfyURL = %q, want empty (notifications off)", cfg.Notify.NtfyURL)
	}
}

func TestLoadConfig_Notify(t *testing.T) {
	setRouterEnv(t)
	t.Setenv("LOOMUX_NTFY_URL", "https://ntfy.example")
	t.Setenv("LOOMUX_NTFY_TOPIC", "loomux")
	t.Setenv("LOOMUX_NTFY_TOKEN", "tk_x")
	t.Setenv("LOOMUX_NOTIFY_EVENTS", "failed,needs_you")
	t.Setenv("LOOMUX_NOTIFY_MIN_DURATION", "2m")
	t.Setenv("LOOMUX_PUBLIC_URL", "https://loomux.example")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	n := cfg.Notify
	if n.NtfyURL != "https://ntfy.example" || n.NtfyTopic != "loomux" || n.NtfyToken != "tk_x" ||
		n.MinDuration != 2*time.Minute || n.PublicURL != "https://loomux.example" ||
		len(n.Events) != 2 || !n.Events["failed"] || !n.Events["needs_you"] {
		t.Fatalf("Notify = %+v", n)
	}
}

func TestLoadConfig_NotifyDefaults(t *testing.T) {
	setRouterEnv(t)
	t.Setenv("LOOMUX_NTFY_URL", "https://ntfy.example")
	t.Setenv("LOOMUX_NTFY_TOPIC", "loomux")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Notify.Events) != 3 || cfg.Notify.MinDuration != defaultNotifyMinDuration {
		t.Fatalf("Notify = %+v, want every event and the default minimum", cfg.Notify)
	}
}

func TestLoadConfig_NotifyInvalid(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"url without topic": {"LOOMUX_NTFY_URL": "https://ntfy.example"},
		"topic without url": {"LOOMUX_NTFY_TOPIC": "loomux"},
		"bad url":           {"LOOMUX_NTFY_URL": "ntfy.example", "LOOMUX_NTFY_TOPIC": "loomux"},
		"unknown event":     {"LOOMUX_NTFY_URL": "https://n.example", "LOOMUX_NTFY_TOPIC": "t", "LOOMUX_NOTIFY_EVENTS": "done,finished"},
		"bad duration":      {"LOOMUX_NTFY_URL": "https://n.example", "LOOMUX_NTFY_TOPIC": "t", "LOOMUX_NOTIFY_MIN_DURATION": "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			setRouterEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig accepted %v", env)
			}
		})
	}
}

func TestLoadConfig_TurnRetention(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil || cfg.TurnRetention != defaultTurnRetention {
		t.Fatalf("default TurnRetention = %v, %v; want %v", cfg.TurnRetention, err, defaultTurnRetention)
	}
	t.Setenv("LOOMUX_TURN_RETENTION", "0")
	if cfg, err := LoadConfig(); err != nil || cfg.TurnRetention != 0 {
		t.Fatalf("TurnRetention=0 = %v, %v; want 0 (keep)", cfg.TurnRetention, err)
	}
	t.Setenv("LOOMUX_TURN_RETENTION", "-1h")
	if _, err := LoadConfig(); err == nil {
		t.Fatalf("a negative retention was accepted")
	}
}
