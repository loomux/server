package app

import (
	"context"
	"fmt"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
)

// App is the fully wired Loomux domain layer, exposed through the one
// operation this package's "minimal internal interface" needs —
// Dispatch — plus lifecycle cleanup and read access to the underlying
// Store for a client-facing layer (api.Server) that needs its own
// session storage in the same database.
type App struct {
	router     *router.Router
	store      *sqlite.Store
	stopReaper context.CancelFunc
	reaperDone chan struct{}
}

// Dispatch routes one chat message through the full pipeline. See
// router.Router.Dispatch. workspaceHint (LOOM-46) is an optional,
// advisory workspace ID — empty means no hint — surfaced as a plain
// string rather than a router.DispatchOption specifically so callers
// like api.Server's Dispatcher interface don't need to import the
// router package just to supply it (mirroring api.Dispatcher's own
// narrow-seam rationale).
func (a *App) Dispatch(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
	var opts []router.DispatchOption
	if workspaceHint != "" {
		opts = append(opts, router.WithWorkspaceHint(workspaceHint))
	}
	return a.router.Dispatch(ctx, conversationID, message, opts...)
}

// RefreshTargetAgents re-probes a target for every agent CLI with a
// declared Binary and records the results (LOOM-71) — see
// router.Router.RefreshTargetAgents. Satisfies api.AgentProber.
func (a *App) RefreshTargetAgents(ctx context.Context, targetID string) ([]*registry.TargetAgent, error) {
	return a.router.RefreshTargetAgents(ctx, targetID)
}

// Store returns the underlying registry.Store — e.g. for api.Server's
// session storage, which must live in the same database as everything
// else rather than a second, separately managed connection.
func (a *App) Store() registry.Store {
	return a.store
}

// Close stops the background idle reaper (LOOM-16) and releases the
// store's resources (its DB connection).
func (a *App) Close() error {
	if a.stopReaper != nil {
		a.stopReaper()
		<-a.reaperDone
	}
	return a.store.Close()
}

// DefaultAgentTypes is the production registered-agent-type set:
// "claude-code" and "codex" (LOOM-22 — proves the agent-type interface
// generalizes beyond a single CLI), both built by package agents. Both
// are TierMarker, and Loomux owns the whole completion path (LOOM-75):
// router.launchAgent tells the launched process its marker path
// (LOOMUX_MARKER_PATH, LOOM-32), and each adapter's CompletionHookArgs
// install the hook that touches it — claude's Stop hook via --settings,
// codex's notify via -c — per launch, with nothing configured on the
// target beforehand. The "" entry configures completion detection for
// shell-kind (provisioning) tasks only (router/agenttype.go's own doc
// comment) — it is never a real dispatchable agent type and is
// deliberately excluded from what's offered to the router model by
// dispatchableAgentTypeNames below.
func DefaultAgentTypes() router.AgentTypeRegistry {
	return router.AgentTypeRegistry{
		"": router.AgentType{
			AgentConfig: completion.AgentConfig{Tier: completion.TierIdle},
		},
		"claude-code": agents.ClaudeCode(),
		"codex":       agents.Codex(),
	}
}

// dispatchableAgentTypeNames excludes the "" bookkeeping entry (see
// DefaultAgentTypes) — the router model must never be offered it as a
// choice, since it isn't a real, launchable agent type.
func dispatchableAgentTypeNames(agentTypes router.AgentTypeRegistry) []string {
	names := make([]string, 0, len(agentTypes))
	for name := range agentTypes {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// Build wires the full domain layer (design spec §2-§7) from cfg:
// storage -> executor factory -> completion detection -> orchestrator
// -> credential resolver -> LLM-backed routing model -> router. On any
// failure after the store opens successfully, the store is closed
// before returning the error.
func Build(cfg Config) (*App, error) {
	return build(cfg, DefaultAgentTypes())
}

// build is Build's parameterized core — split out so tests can wire a
// custom AgentTypeRegistry (e.g. a fast TierIdle agent type against a
// real tmux session) without needing a real "claude" CLI installed,
// while Build's public signature stays fixed to the production
// registry.
func build(cfg Config, agentTypes router.AgentTypeRegistry) (*App, error) {
	var opts []sqlite.Option
	if cfg.MasterKey != nil {
		opts = append(opts, sqlite.WithMasterKey(cfg.MasterKey))
	}
	store, err := sqlite.Open(cfg.DBPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}

	// Passed to both the detector and the router, so they agree on where
	// a TierMarker agent-type's marker files live (LOOM-32): router.
	// launchAgent tells the launched process the path, completion.Detector
	// watches it. Empty means each target's per-user default, which both
	// resolve the same way (completion.ResolveMarkerDir).
	markerDir := cfg.MarkerDir
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	creds := credentials.NewResolver(store)

	model, err := llmrouter.New(cfg.Router, dispatchableAgentTypeNames(agentTypes))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("app: router model: %w", err)
	}

	threshold := cfg.ReapIdleThreshold
	if threshold == 0 {
		threshold = defaultReapIdleThreshold
	}
	interval := cfg.ReapInterval
	if interval == 0 {
		interval = defaultReapInterval
	}
	var routerOpts []router.Option
	if cfg.Logger != nil {
		routerOpts = append(routerOpts, router.WithLogger(cfg.Logger))
	}

	reaper := orchestrator.NewReaper(orch, threshold)
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		reaper.Run(reaperCtx, interval)
	}()

	return &App{
		router:     router.New(store, orch, targets.NewExecutor, creds, agentTypes, model, markerDir, routerOpts...),
		store:      store,
		stopReaper: stopReaper,
		reaperDone: reaperDone,
	}, nil
}
