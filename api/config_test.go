package api_test

import (
	"testing"
	"time"

	"github.com/Loomux/server/api"
)

func validPasswordHashEnv(t *testing.T) string {
	t.Helper()
	hash, err := api.HashPassword("some-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.SessionTTL != 30*24*time.Hour {
		t.Errorf("SessionTTL = %v, want 30 days", cfg.SessionTTL)
	}
	if len(cfg.PasswordHash) == 0 {
		t.Error("PasswordHash is empty")
	}
}

func TestLoadConfig_MissingPasswordHash(t *testing.T) {
	_, err := api.LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig: want error when LOOMUX_AUTH_PASSWORD_HASH is unset")
	}
}

func TestLoadConfig_InvalidPasswordHash(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", "not-a-bcrypt-hash")
	_, err := api.LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig: want error for a malformed bcrypt hash")
	}
}

func TestLoadConfig_CustomAddrAndTTL(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("LOOMUX_SESSION_TTL", "1h")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9090" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "127.0.0.1:9090")
	}
	if cfg.SessionTTL != time.Hour {
		t.Errorf("SessionTTL = %v, want 1h", cfg.SessionTTL)
	}
}

func TestLoadConfig_InvalidSessionTTL(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_SESSION_TTL", "not-a-duration")

	_, err := api.LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig: want error for a malformed session TTL")
	}
}

func TestLoadConfig_StaticDirDefaultsEmpty(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.StaticDir != "" {
		t.Errorf("StaticDir = %q, want empty (static serving disabled by default)", cfg.StaticDir)
	}
}

func TestLoadConfig_StaticDirFromEnv(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_STATIC_DIR", "/srv/loomux/web")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.StaticDir != "/srv/loomux/web" {
		t.Errorf("StaticDir = %q, want %q", cfg.StaticDir, "/srv/loomux/web")
	}
}
