package api

import (
	"fmt"
	"os"
	"path/filepath"
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
	// SessionMaxAge is a session's absolute lifetime, 0 for none (see
	// WithSessionMaxAge).
	SessionMaxAge time.Duration
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
	// WebBundlesDir, if set together with StaticDir, is where web client
	// updates are installed (LOOM-118): StaticDir is then the image's own
	// bundle, served until a newer one is installed here. It must persist
	// across restarts, so it defaults to web-bundles next to the database
	// (LOOMUX_DB_PATH), which lives on the data volume.
	WebBundlesDir string
	// WebUpdates is how the web client is updated in place (LOOM-118):
	// "attested" installs the newest release loomux/web's CI built on
	// main, proven by its build-provenance attestation; "pinned" installs
	// what loomux/server main pins; "off" installs nothing (the bundle
	// served is still reported). Unset: "attested" if WebReleasesToken is
	// set (how the feature was switched on before), otherwise "off".
	WebUpdates string
	// WebReleasesToken is an optional read-only GitHub token: both
	// repositories are public, so it only raises the API rate limit.
	WebReleasesToken string
	// WebPinRepo is the repository whose main pins the bundle to serve
	// (deploy/web-ref, deploy/web-sha256): loomux/server.
	WebPinRepo string
	// WebReleasesRepo is the repository web bundles are published in.
	WebReleasesRepo string
}

const (
	envPasswordHash = "LOOMUX_AUTH_PASSWORD_HASH"
	envAddr         = "LOOMUX_HTTP_ADDR"
	envSessionTTL   = "LOOMUX_SESSION_TTL"
	envSessionMax   = "LOOMUX_SESSION_MAX_AGE"
	envStaticDir    = "LOOMUX_STATIC_DIR"
	envMetricsAddr  = "LOOMUX_METRICS_ADDR"
	envWebBundles   = "LOOMUX_WEB_BUNDLES_DIR"
	envWebToken     = "LOOMUX_WEB_RELEASES_TOKEN"
	envWebRepo      = "LOOMUX_WEB_RELEASES_REPO"
	envWebPinRepo   = "LOOMUX_WEB_PIN_REPO"
	envWebUpdates   = "LOOMUX_WEB_UPDATES"
	// envDBPath is app's (app.Config); read here only to place
	// WebBundlesDir beside the database.
	envDBPath = "LOOMUX_DB_PATH"

	defaultAddr        = ":8080"
	defaultMetricsAddr = "127.0.0.1:9090"
	defaultWebRepo     = "loomux/web"
	defaultWebPinRepo  = "loomux/server"
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
	maxAge := defaultSessionMaxAge
	if raw := os.Getenv(envSessionMax); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("api: %s must be a duration (0 for no limit), not %q", envSessionMax, raw)
		}
		maxAge = d
	}

	metricsAddr := defaultMetricsAddr
	if raw, ok := os.LookupEnv(envMetricsAddr); ok {
		// Explicitly set, even to "", means the caller chose the value.
		// An empty string disables the metrics endpoint.
		metricsAddr = raw
	}

	webBundles := os.Getenv(envWebBundles)
	if db := os.Getenv(envDBPath); webBundles == "" && db != "" {
		webBundles = filepath.Join(filepath.Dir(db), "web-bundles")
	}
	webRepo := os.Getenv(envWebRepo)
	if webRepo == "" {
		webRepo = defaultWebRepo
	}
	webUpdates := os.Getenv(envWebUpdates)
	switch {
	case webUpdates == "" && os.Getenv(envWebToken) != "":
		webUpdates = "attested"
	case webUpdates == "":
		webUpdates = "off"
	case webUpdates != "off" && webUpdates != "attested" && webUpdates != "pinned":
		return Config{}, fmt.Errorf("api: %s must be off, attested or pinned, not %q", envWebUpdates, webUpdates)
	}
	webPinRepo := os.Getenv(envWebPinRepo)
	if webPinRepo == "" {
		webPinRepo = defaultWebPinRepo
	}

	return Config{
		PasswordHash: []byte(hash), Addr: addr, SessionTTL: ttl, SessionMaxAge: maxAge, StaticDir: os.Getenv(envStaticDir), MetricsAddr: metricsAddr,
		WebBundlesDir: webBundles, WebUpdates: webUpdates, WebReleasesToken: os.Getenv(envWebToken), WebPinRepo: webPinRepo, WebReleasesRepo: webRepo,
	}, nil
}
