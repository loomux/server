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

func TestLoadConfig_EventRetention(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil || cfg.EventRetention != defaultEventRetention {
		t.Fatalf("default EventRetention = %v, %v; want %v", cfg.EventRetention, err, defaultEventRetention)
	}
	t.Setenv("LOOMUX_EVENT_RETENTION", "0")
	if cfg, err := LoadConfig(); err != nil || cfg.EventRetention != 0 {
		t.Fatalf("EventRetention=0 = %v, %v; want 0 (keep)", cfg.EventRetention, err)
	}
	t.Setenv("LOOMUX_EVENT_RETENTION", "-1h")
	if _, err := LoadConfig(); err == nil {
		t.Fatalf("a negative retention was accepted")
	}
}

// LOOM-193: sessions, dispatches and confirmations are kept 7, 90 and
// 30 days by default; each setting takes a duration, 0 keeps the rows,
// and a negative or malformed one is a startup error.
func TestLoadConfig_RowRetention(t *testing.T) {
	for _, tc := range []struct {
		env  string
		def  time.Duration
		read func(Config) time.Duration
	}{
		{"LOOMUX_SESSION_RETENTION", 7 * 24 * time.Hour, func(c Config) time.Duration { return c.SessionRetention }},
		{"LOOMUX_DISPATCH_RETENTION", 90 * 24 * time.Hour, func(c Config) time.Duration { return c.DispatchRetention }},
		{"LOOMUX_CONFIRMATION_RETENTION", 30 * 24 * time.Hour, func(c Config) time.Duration { return c.ConfirmationRetention }},
	} {
		t.Run(tc.env, func(t *testing.T) {
			setRouterEnv(t)
			if cfg, err := LoadConfig(); err != nil || tc.read(cfg) != tc.def {
				t.Fatalf("default = %v, %v; want %v", tc.read(cfg), err, tc.def)
			}
			for raw, want := range map[string]time.Duration{"0": 0, "48h": 48 * time.Hour} {
				t.Setenv(tc.env, raw)
				if cfg, err := LoadConfig(); err != nil || tc.read(cfg) != want {
					t.Errorf("%s=%s: %v, %v; want %v", tc.env, raw, tc.read(cfg), err, want)
				}
			}
			for _, raw := range []string{"-1h", "7d", "soon"} {
				t.Setenv(tc.env, raw)
				if _, err := LoadConfig(); err == nil {
					t.Errorf("%s=%s was accepted", tc.env, raw)
				}
			}
		})
	}
}

// A negative dispatch ceiling or drain is a startup error, like the
// retention durations beside them (#256 review).
func TestLoadConfig_DispatchDurationsNotNegative(t *testing.T) {
	setRouterEnv(t)
	for _, v := range []string{"LOOMUX_DISPATCH_MAX_DURATION", "LOOMUX_DISPATCH_DRAIN"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(v, "-5m")
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), v) {
				t.Fatalf("%s=-5m: err = %v, want it refused", v, err)
			}
			t.Setenv(v, "90s")
			if _, err := LoadConfig(); err != nil {
				t.Fatalf("%s=90s: %v", v, err)
			}
		})
	}
}

// Two instances on one target each need their own tmux socket, or each
// one's orphan sweep reaps the other's sessions.
func TestLoadConfig_TmuxSocket(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil || cfg.TmuxSocket != "" {
		t.Fatalf("default: TmuxSocket = %q, %v; want empty (targets' default)", cfg.TmuxSocket, err)
	}
	t.Setenv(envTmuxSocket, "loomux-prod")
	if cfg, err = LoadConfig(); err != nil || cfg.TmuxSocket != "loomux-prod" {
		t.Fatalf("TmuxSocket = %q, %v; want loomux-prod", cfg.TmuxSocket, err)
	}
	for _, bad := range []string{"loomux prod", "a;rm -rf x", "-L", "x/y"} {
		t.Setenv(envTmuxSocket, bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("%s=%q: got nil error", envTmuxSocket, bad)
		}
	}
}

// LOOM-141: LOOMUX_LOCAL_TARGETS is on unless set off; anything else is
// a configuration error, not a silent default.
func TestLoadConfig_LocalTargets(t *testing.T) {
	for raw, wantOff := range map[string]bool{"": false, "on": false, "true": false, "off": true, "OFF": true, "false": true, "0": true} {
		setRouterEnv(t)
		t.Setenv("LOOMUX_LOCAL_TARGETS", raw)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.LocalTargetsOff != wantOff {
			t.Errorf("%q: LocalTargetsOff = %v, want %v", raw, cfg.LocalTargetsOff, wantOff)
		}
	}
	setRouterEnv(t)
	t.Setenv("LOOMUX_LOCAL_TARGETS", "maybe")
	if _, err := LoadConfig(); err == nil {
		t.Error(`LOOMUX_LOCAL_TARGETS="maybe" was accepted`)
	}
}
