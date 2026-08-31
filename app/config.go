// Package app is Loomux's composition root: it wires the workspace
// registry, TargetExecutor, orchestrator, completion detection,
// credential vault, and router dispatch pipeline (design spec §2-§7)
// into a runnable App with a single Dispatch operation. The versioned
// HTTP/WS client API and login-gated auth (design spec §9, §10 axis 1,
// LOOM-9) are explicitly out of scope here — this package only proves
// the whole stack constructs and is dispatchable; a later HTTP/WS/auth
// layer is expected to wrap an *App rather than re-wire these pieces
// itself. cmd/loomuxd is the thin CLI entrypoint built on this package.
package app

import (
	"fmt"
	"os"
	"time"

	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router/llmrouter"
)

// Config is the process configuration, loaded from environment
// variables (this repo's established convention — see
// registry/sqlite.KeyFromEnv, router/llmrouter.ConfigFromEnv).
type Config struct {
	// DBPath is the SQLite database file path. Defaults to "loomux.db"
	// in the working directory if unset — not a secret, so (unlike the
	// fail-fast-on-malformed values below) a default is appropriate.
	DBPath string
	// MarkerDir is completion.NewDetector's marker file directory.
	// Empty uses completion's own package default.
	MarkerDir string
	// MasterKey is the credential vault's AES-256 key, or nil if
	// LOOMUX_MASTER_KEY is unset. Optional at this layer: registry/sqlite
	// already fails fast on any credential operation that actually needs
	// it, rather than this process refusing to start without one.
	MasterKey []byte
	// Router is the LLM-backed RoutingModel's own configuration.
	Router llmrouter.Config
	// ReapIdleThreshold is how long a task can go without a state change
	// (design spec's continuation model, LOOM-16 — see
	// orchestrator.Reaper's doc comment for exactly what "idle" means)
	// before its session is torn down. Zero means "unset" — build applies
	// defaultReapIdleThreshold, the same way LoadConfig does; existing
	// callers that construct Config directly (tests) get sensible
	// behavior without needing to set every field.
	ReapIdleThreshold time.Duration
	// ReapInterval is how often the idle reaper sweeps. Same zero-means-
	// default handling as ReapIdleThreshold.
	ReapInterval time.Duration
}

const (
	envDBPath            = "LOOMUX_DB_PATH"
	envMarkerDir         = "LOOMUX_MARKER_DIR"
	envMasterKey         = "LOOMUX_MASTER_KEY"
	envReapIdleThreshold = "LOOMUX_REAP_IDLE_THRESHOLD"
	envReapInterval      = "LOOMUX_REAP_INTERVAL"

	defaultDBPath = "loomux.db"

	// defaultReapIdleThreshold/Interval are also build's fallback for a
	// zero Config field, not just LoadConfig's env default — see Config's
	// doc comments.
	defaultReapIdleThreshold = 24 * time.Hour
	defaultReapInterval      = time.Hour
)

// LoadConfig reads Config from the environment, failing fast on
// malformed (as opposed to merely absent) values — an unset
// LOOMUX_MASTER_KEY is fine (MasterKey stays nil); an invalid one is
// not.
func LoadConfig() (Config, error) {
	dbPath := os.Getenv(envDBPath)
	if dbPath == "" {
		dbPath = defaultDBPath
	}

	var masterKey []byte
	if os.Getenv(envMasterKey) != "" {
		key, err := sqlite.KeyFromEnv(envMasterKey)
		if err != nil {
			return Config{}, fmt.Errorf("app: %w", err)
		}
		masterKey = key
	}

	routerCfg, err := llmrouter.ConfigFromEnv()
	if err != nil {
		return Config{}, fmt.Errorf("app: router config: %w", err)
	}

	reapIdleThreshold := defaultReapIdleThreshold
	if raw := os.Getenv(envReapIdleThreshold); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("app: %s is not a valid duration: %w", envReapIdleThreshold, err)
		}
		reapIdleThreshold = d
	}
	reapInterval := defaultReapInterval
	if raw := os.Getenv(envReapInterval); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("app: %s is not a valid duration: %w", envReapInterval, err)
		}
		reapInterval = d
	}

	return Config{
		DBPath:            dbPath,
		MarkerDir:         os.Getenv(envMarkerDir),
		MasterKey:         masterKey,
		Router:            routerCfg,
		ReapIdleThreshold: reapIdleThreshold,
		ReapInterval:      reapInterval,
	}, nil
}
