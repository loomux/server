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

func TestLoadConfig_MetricsAddrDefaultsTo9090(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("MetricsAddr = %q, want %q", cfg.MetricsAddr, "127.0.0.1:9090")
	}
}

func TestLoadConfig_MetricsAddrFromEnv(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_METRICS_ADDR", ":9090")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Errorf("MetricsAddr = %q, want %q", cfg.MetricsAddr, ":9090")
	}
}

func TestLoadConfig_MetricsAddrEmptyDisables(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_METRICS_ADDR", "")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MetricsAddr != "" {
		t.Errorf("MetricsAddr = %q, want empty string (disabled)", cfg.MetricsAddr)
	}
}

// LOOM-118: web updates install beside the database (the data volume)
// unless told otherwise; the token is optional.
func TestLoadConfig_WebUpdates(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_DB_PATH", "/data/loomux.db")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.WebBundlesDir != "/data/web-bundles" || cfg.WebReleasesRepo != "loomux/web" || cfg.WebPinRepo != "loomux/server" ||
		cfg.WebReleasesToken != "" {
		t.Errorf("web config = %+v", cfg)
	}

	t.Setenv("LOOMUX_WEB_BUNDLES_DIR", "/elsewhere")
	t.Setenv("LOOMUX_WEB_RELEASES_TOKEN", "tok")
	if cfg, _ := api.LoadConfig(); cfg.WebBundlesDir != "/elsewhere" || cfg.WebReleasesToken != "tok" {
		t.Errorf("overridden web config = %q, %q", cfg.WebBundlesDir, cfg.WebReleasesToken)
	}
}

// LOOM-118: how the web client updates. Unset follows the token (how it
// was switched on before); anything else is an error.
func TestLoadConfig_WebUpdatesMode(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	for _, tc := range []struct{ updates, token, want string }{
		{"", "", "off"}, {"", "tok", "attested"}, {"pinned", "", "pinned"}, {"off", "tok", "off"},
	} {
		t.Setenv("LOOMUX_WEB_UPDATES", tc.updates)
		t.Setenv("LOOMUX_WEB_RELEASES_TOKEN", tc.token)
		cfg, err := api.LoadConfig()
		if err != nil || cfg.WebUpdates != tc.want {
			t.Errorf("updates %q token %q: %q, %v; want %q", tc.updates, tc.token, cfg.WebUpdates, err, tc.want)
		}
	}
	t.Setenv("LOOMUX_WEB_UPDATES", "always")
	if _, err := api.LoadConfig(); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
