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
}

const (
	envDBPath    = "LOOMUX_DB_PATH"
	envMarkerDir = "LOOMUX_MARKER_DIR"
	envMasterKey = "LOOMUX_MASTER_KEY"

	defaultDBPath = "loomux.db"
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

	return Config{
		DBPath:    dbPath,
		MarkerDir: os.Getenv(envMarkerDir),
		MasterKey: masterKey,
		Router:    routerCfg,
	}, nil
}
