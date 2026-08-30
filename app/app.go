package app

import (
	"context"
	"fmt"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
)

// App is the fully wired Loomux domain layer, exposed through the one
// operation this package's "minimal internal interface" needs —
// Dispatch — plus lifecycle cleanup.
type App struct {
	router *router.Router
	store  *sqlite.Store
}

// Dispatch routes one chat message through the full pipeline. See
// router.Router.Dispatch.
func (a *App) Dispatch(ctx context.Context, conversationID, message string) (string, error) {
	return a.router.Dispatch(ctx, conversationID, message)
}

// Close releases the store's resources (its DB connection).
func (a *App) Close() error {
	return a.store.Close()
}

// DefaultAgentTypes is the production registered-agent-type set. Only
// "claude-code" is wired today — matches this repo's own established
// test-fixture convention (router/agenttype_test.go) and the fact that
// this whole project is built around tmux-backed Claude Code agents.
// The "" entry configures completion detection for shell-kind
// (provisioning) tasks only (router/agenttype.go's own doc comment) —
// it is never a real dispatchable agent type and is deliberately
// excluded from what's offered to the router model by
// dispatchableAgentTypeNames below.
func DefaultAgentTypes() router.AgentTypeRegistry {
	return router.AgentTypeRegistry{
		"": router.AgentType{
			AgentConfig: completion.AgentConfig{Tier: completion.TierIdle},
		},
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker},
			LaunchTemplate: "claude",
		},
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

	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), cfg.MarkerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	creds := credentials.NewResolver(store)

	model, err := llmrouter.New(cfg.Router, dispatchableAgentTypeNames(agentTypes))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("app: router model: %w", err)
	}

	return &App{
		router: router.New(store, orch, targets.NewExecutor, creds, agentTypes, model),
		store:  store,
	}, nil
}
