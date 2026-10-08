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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
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
	// TmuxSocket is the tmux socket name every session runs on
	// (LOOMUX_TMUX_SOCKET); empty means targets.DefaultTmuxSocket. Give
	// two instances driving the same targets different ones.
	TmuxSocket string
	// LocalTargetsOff (LOOMUX_LOCAL_TARGETS=off; the container image sets
	// it) turns local targets off (LOOM-141): a local target's agents run
	// as loomuxd's own user, so they're fully trusted. The zero value
	// leaves them on. See targets.LocalTargets.
	LocalTargetsOff bool
	// MasterKey is the credential vault's AES-256 key, or nil if
	// LOOMUX_MASTER_KEY is unset. Optional at this layer: registry/sqlite
	// already fails fast on any credential operation that actually needs
	// it, rather than this process refusing to start without one.
	MasterKey []byte
	// SSHProxy is the SOCKS5 proxy (host:port) managed targets are
	// reached through unless they opt out (LOOMUX_SSH_PROXY, as
	// socks5://host:port; LOOM-138); "" for none.
	SSHProxy string
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
	// TargetProbeInterval is how often every target's health is probed
	// (LOOM-86). Same zero-means-default handling as ReapIdleThreshold.
	TargetProbeInterval time.Duration
	// Logger receives the router's structured records (LOOM-63). Nil —
	// as in tests that construct Config directly — means nothing is
	// logged; LoadConfig always sets one.
	Logger *slog.Logger
	// AgentProfiles overrides agent-types' launch profiles (LOOM-78):
	// permission/sandbox args, workspace pre-trust, first prompt as an
	// argument. Keyed by agent-type name. Nil keeps every default (see
	// agents/README.md); naming an unregistered agent-type fails Build.
	AgentProfiles map[string]router.ProfileOverride
	// DispatchMaxDuration bounds one dispatch job end to end (LOOM-80).
	// Zero means the dispatch package's own default.
	DispatchMaxDuration time.Duration
	// DispatchDrain is how long shutdown lets in-flight dispatch jobs
	// finish before marking them interrupted (LOOM-80). Zero means
	// defaultDispatchDrain.
	DispatchDrain time.Duration
	// Notify configures turn notifications (LOOM-102); off when
	// Notify.NtfyURL is empty.
	Notify NotifyConfig
	// TurnRetention is how long each task turn's transcript is kept
	// (LOOM-122); zero keeps them. LoadConfig defaults it to
	// defaultTurnRetention.
	TurnRetention time.Duration
	// EventRetention is how long the dispatch audit trail is kept
	// (LOOM-110); zero keeps it. LoadConfig defaults it to
	// defaultEventRetention.
	EventRetention time.Duration
}

// NotifyConfig configures turn notifications (LOOM-102).
type NotifyConfig struct {
	// NtfyURL is the ntfy server ("https://ntfy.sh"), NtfyTopic the topic
	// posted to and NtfyToken an optional access token. An empty NtfyURL
	// turns notifications off.
	NtfyURL, NtfyTopic, NtfyToken string
	// Events are the kinds notified ("done", "failed", "needs_you").
	Events map[notify.Kind]bool
	// MinDuration skips turns shorter than this: the user was likely
	// still watching.
	MinDuration time.Duration
	// PublicURL is where the web client is served, for each
	// notification's link to its conversation; empty means no link.
	PublicURL string
}

const (
	envDBPath              = "LOOMUX_DB_PATH"
	envMarkerDir           = "LOOMUX_MARKER_DIR"
	envTmuxSocket          = "LOOMUX_TMUX_SOCKET"
	envLocalTargets        = "LOOMUX_LOCAL_TARGETS"
	envMasterKey           = "LOOMUX_MASTER_KEY"
	envSSHProxy            = "LOOMUX_SSH_PROXY"
	envReapIdleThreshold   = "LOOMUX_REAP_IDLE_THRESHOLD"
	envReapInterval        = "LOOMUX_REAP_INTERVAL"
	envTargetProbeInterval = "LOOMUX_TARGET_PROBE_INTERVAL"
	envLogLevel            = "LOOMUX_LOG_LEVEL"
	envAgentProfiles       = "LOOMUX_AGENT_PROFILES"
	envDispatchMaxDuration = "LOOMUX_DISPATCH_MAX_DURATION"
	envDispatchDrain       = "LOOMUX_DISPATCH_DRAIN"
	envNtfyURL             = "LOOMUX_NTFY_URL"
	envNtfyTopic           = "LOOMUX_NTFY_TOPIC"
	envNtfyToken           = "LOOMUX_NTFY_TOKEN"
	envNotifyEvents        = "LOOMUX_NOTIFY_EVENTS"
	envNotifyMinDuration   = "LOOMUX_NOTIFY_MIN_DURATION"
	envPublicURL           = "LOOMUX_PUBLIC_URL"
	envTurnRetention       = "LOOMUX_TURN_RETENTION"
	envEventRetention      = "LOOMUX_EVENT_RETENTION"

	defaultDBPath = "loomux.db"

	// defaultReapIdleThreshold/Interval are also build's fallback for a
	// zero Config field, not just LoadConfig's env default — see Config's
	// doc comments.
	defaultReapIdleThreshold   = 24 * time.Hour
	defaultReapInterval        = time.Hour
	defaultTargetProbeInterval = 5 * time.Minute
	// defaultDispatchDrain fits inside Kubernetes' default 30s
	// termination grace, leaving room for the HTTP server's own shutdown.
	defaultDispatchDrain = 20 * time.Second
	// defaultNotifyMinDuration: a turn quicker than this was most likely
	// watched as it happened.
	defaultNotifyMinDuration = 30 * time.Second
	// defaultTurnRetention keeps a month of per-turn transcripts: each
	// can carry a 256 KiB pane.
	defaultTurnRetention = 30 * 24 * time.Hour
	// defaultEventRetention keeps 90 days of the dispatch audit trail:
	// small rows, read after the fact.
	defaultEventRetention = 90 * 24 * time.Hour
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
	targetProbeInterval := defaultTargetProbeInterval
	if raw := os.Getenv(envTargetProbeInterval); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("app: %s is not a valid positive duration: %q", envTargetProbeInterval, raw)
		}
		targetProbeInterval = d
	}

	var dispatchMaxDuration time.Duration
	if raw := os.Getenv(envDispatchMaxDuration); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("app: %s is not a valid duration (0 means the default): %q", envDispatchMaxDuration, raw)
		}
		dispatchMaxDuration = d
	}
	dispatchDrain := defaultDispatchDrain
	if raw := os.Getenv(envDispatchDrain); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("app: %s is not a valid duration: %q", envDispatchDrain, raw)
		}
		dispatchDrain = d
	}

	turnRetention := defaultTurnRetention
	if raw := os.Getenv(envTurnRetention); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("app: %s is not a valid duration (0 keeps turns): %q", envTurnRetention, raw)
		}
		turnRetention = d
	}

	eventRetention := defaultEventRetention
	if raw := os.Getenv(envEventRetention); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("app: %s is not a valid duration (0 keeps events): %q", envEventRetention, raw)
		}
		eventRetention = d
	}

	var logLevel slog.Level
	if raw := os.Getenv(envLogLevel); raw != "" {
		if err := logLevel.UnmarshalText([]byte(raw)); err != nil {
			return Config{}, fmt.Errorf("app: %s must be debug, info, warn or error: %w", envLogLevel, err)
		}
	}

	var agentProfiles map[string]router.ProfileOverride
	if raw := os.Getenv(envAgentProfiles); raw != "" {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields() // a misspelt key must not leave the default posture in force
		if err := dec.Decode(&agentProfiles); err != nil {
			return Config{}, fmt.Errorf("app: %s is not a valid profile override map: %w", envAgentProfiles, err)
		}
	}

	notifyCfg, err := loadNotifyConfig()
	if err != nil {
		return Config{}, err
	}

	sshProxy, err := parseSSHProxy(os.Getenv(envSSHProxy))
	if err != nil {
		return Config{}, err
	}

	tmuxSocket := os.Getenv(envTmuxSocket)
	if tmuxSocket != "" && !targets.ValidTmuxSocket(tmuxSocket) {
		return Config{}, fmt.Errorf("app: %s %q: letters, digits, '-' and '_' only", envTmuxSocket, tmuxSocket)
	}

	localTargetsOff := false
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(envLocalTargets))); v {
	case "", "on", "true", "1", "yes":
	case "off", "false", "0", "no":
		localTargetsOff = true
	default:
		return Config{}, fmt.Errorf("app: %s %q: on or off", envLocalTargets, v)
	}

	return Config{
		Notify:              notifyCfg,
		LocalTargetsOff:     localTargetsOff,
		AgentProfiles:       agentProfiles,
		DBPath:              dbPath,
		MarkerDir:           os.Getenv(envMarkerDir),
		TmuxSocket:          tmuxSocket,
		MasterKey:           masterKey,
		SSHProxy:            sshProxy,
		Router:              routerCfg,
		ReapIdleThreshold:   reapIdleThreshold,
		ReapInterval:        reapInterval,
		TargetProbeInterval: targetProbeInterval,
		DispatchMaxDuration: dispatchMaxDuration,
		DispatchDrain:       dispatchDrain,
		TurnRetention:       turnRetention,
		EventRetention:      eventRetention,
		// JSON on stderr: one record per line, for the container log.
		Logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})),
	}, nil
}

// parseSSHProxy reads LOOMUX_SSH_PROXY: socks5://host:port, no
// credentials, path or query. It returns host:port, which goes into
// managed targets' ProxyCommand (targets.SetManagedSSH vets it again).
func parseSSHProxy(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "socks5" || u.User != nil || u.Port() == "" || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("app: %s must be socks5://host:port: %q", envSSHProxy, raw)
	}
	for _, r := range u.Hostname() {
		if !(r == '.' || r == '-' || r == ':' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "", fmt.Errorf("app: %s must be socks5://host:port: %q", envSSHProxy, raw)
		}
	}
	return u.Host, nil
}

// loadNotifyConfig reads NotifyConfig (LOOM-102) from the environment.
func loadNotifyConfig() (NotifyConfig, error) {
	cfg := NotifyConfig{
		NtfyURL:     os.Getenv(envNtfyURL),
		NtfyTopic:   os.Getenv(envNtfyTopic),
		NtfyToken:   os.Getenv(envNtfyToken),
		MinDuration: defaultNotifyMinDuration,
		PublicURL:   os.Getenv(envPublicURL),
	}
	if cfg.NtfyURL == "" && cfg.NtfyTopic == "" {
		return NotifyConfig{}, nil
	}
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return NotifyConfig{}, fmt.Errorf("app: %s and %s must be set together", envNtfyURL, envNtfyTopic)
	}
	if u, err := url.Parse(cfg.NtfyURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return NotifyConfig{}, fmt.Errorf("app: %s must be an http(s) URL: %q", envNtfyURL, cfg.NtfyURL)
	}
	events := "done,failed,needs_you"
	if raw := os.Getenv(envNotifyEvents); raw != "" {
		events = raw
	}
	kinds, err := notify.ParseKinds(events)
	if err != nil {
		return NotifyConfig{}, fmt.Errorf("app: %s: %w", envNotifyEvents, err)
	}
	cfg.Events = kinds
	if raw := os.Getenv(envNotifyMinDuration); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return NotifyConfig{}, fmt.Errorf("app: %s is not a valid duration: %q", envNotifyMinDuration, raw)
		}
		cfg.MinDuration = d
	}
	return cfg, nil
}
