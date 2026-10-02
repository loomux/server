package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLoadConfig_LogLevelDefaultsToInfo(t *testing.T) {
	setRouterEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Logger == nil {
		t.Fatalf("Logger = nil, want a logger")
	}
	if !cfg.Logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("info not enabled by default")
	}
	if cfg.Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("debug enabled by default, want info")
	}
}

func TestLoadConfig_LogLevelFromEnv(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envLogLevel, "debug")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("debug not enabled with %s=debug", envLogLevel)
	}
}

func TestLoadConfig_InvalidLogLevel(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envLogLevel, "chatty")

	if _, err := LoadConfig(); err == nil {
		t.Fatalf("LoadConfig with %s=chatty: got nil error", envLogLevel)
	}
}

// TestBuild_WiresLoggerIntoRouter proves Config.Logger actually reaches
// the dispatch pipeline (LOOM-63) — the gap the issue was filed for was
// a production log with nothing between "listening" and silence.
func TestBuild_WiresLoggerIntoRouter(t *testing.T) {
	srv := fakeRouterServer(t)
	var buf bytes.Buffer
	cfg := testConfig(t, srv.URL)
	cfg.Logger = slog.New(slog.NewJSONHandler(&buf, nil))

	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()

	if _, err := app.Dispatch(context.Background(), "conv-1", "hi", ""); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"routing decision"`) {
		t.Errorf("no routing decision logged; got: %s", buf.String())
	}
}
