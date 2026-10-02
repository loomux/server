package api

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Config is api's own process configuration, loaded from environment
// variables — this repo's established convention (see
// registry/sqlite.KeyFromEnv, router/llmrouter.ConfigFromEnv,
// app.LoadConfig).
type Config struct {
	// PasswordHash is a bcrypt hash (see HashPassword) — the single v1
	// user's credential. Required: there is no unauthenticated fallback
	// (design spec §9: "no 'it's just me so no auth' shortcut").
	PasswordHash []byte
	// Addr is the address Server listens on. Plain HTTP is assumed — the
	// deployment model is a reverse proxy in front terminating TLS (see
	// README.md).
	Addr string
	// SessionTTL is the sliding-expiration window (see WithSessionTTL).
	SessionTTL time.Duration
	// StaticDir, if set, is the directory containing the web client's
	// built static files (loomux/web's Vite build output) — passed to
	// WithStaticDir so Server serves it for any non-/api/* path, with SPA
	// fallback to index.html (design spec docs/design/web-client-design.md,
	// "Hosting / serving integration"; tracked as GitHub issue LOOM-33).
	// Empty (the default) disables static serving entirely — the same
	// "optional, unvalidated path" convention as app.Config.MarkerDir,
	// since there's no way to tell "not configured yet" apart from "wrong
	// path" without trying to open it, and that's Server's job, not
	// LoadConfig's.
	StaticDir string
	// MetricsAddr is the address the Prometheus metrics scraper listens
	// on (LOOM-103). Empty disables the endpoint entirely. The default
	// binds to loopback only (127.0.0.1:9090); production deployments that
	// need in-cluster scraping must set this explicitly to ":9090" or a
	// pod IP.
	MetricsAddr string
}

const (
	envPasswordHash = "LOOMUX_AUTH_PASSWORD_HASH"
	envAddr         = "LOOMUX_HTTP_ADDR"
	envSessionTTL   = "LOOMUX_SESSION_TTL"
	envStaticDir    = "LOOMUX_STATIC_DIR"
	envMetricsAddr  = "LOOMUX_METRICS_ADDR"

	defaultAddr        = ":8080"
	defaultMetricsAddr = "127.0.0.1:9090"
)

// LoadConfig reads Config from the environment, failing fast on a
// missing or malformed password hash and on a malformed (not merely
// absent) session TTL.
func LoadConfig() (Config, error) {
	hash := os.Getenv(envPasswordHash)
	if hash == "" {
		return Config{}, fmt.Errorf("api: %s is not set", envPasswordHash)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return Config{}, fmt.Errorf("api: %s is not a valid bcrypt hash: %w", envPasswordHash, err)
	}

	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}

	ttl := defaultSessionTTL
	if raw := os.Getenv(envSessionTTL); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("api: %s is not a valid duration: %w", envSessionTTL, err)
		}
		ttl = d
	}

	metricsAddr := defaultMetricsAddr
	if raw, ok := os.LookupEnv(envMetricsAddr); ok {
		// Explicitly set, even to "", means the caller chose the value.
		// An empty string disables the metrics endpoint.
		metricsAddr = raw
	}

	return Config{PasswordHash: []byte(hash), Addr: addr, SessionTTL: ttl, StaticDir: os.Getenv(envStaticDir), MetricsAddr: metricsAddr}, nil
}
