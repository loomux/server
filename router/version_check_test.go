package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// versionCheckedAgentTypes mirrors shortIdleAgentTypes but with a
// VersionCheck attached to "claude-code", for the version-check-specific
// tests below.
func versionCheckedAgentTypes(vc router.VersionCheck) router.AgentTypeRegistry {
	short := completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}
	return router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: short},
		"claude-code": router.AgentType{
			AgentConfig:    short,
			LaunchTemplate: "claude",
			VersionCheck:   &vc,
		},
	}
}

func setupWithVersionCheck(t *testing.T, vc router.VersionCheck) (registry.Store, *fakeExecutor, *router.Router, *routertest.StubRoutingModel) {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	agentTypes := versionCheckedAgentTypes(vc)
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{}
	r := router.New(store, orch, exec.factory(), resolver, agentTypes, model, markerDir)
	return store, exec, r, model
}

func TestDispatch_VersionCheckPasses_LaunchesNormally(t *testing.T) {
	store, exec, r, model := setupWithVersionCheck(t, router.VersionCheck{
		Command: "claude --version",
		Parse:   router.ExtractDottedVersion,
		Min:     "2.0.0",
	})
	ws := createFixtureWorkspace(t, store)

	exec.runOnceOutput = "2.1.251 (Claude Code)"

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "ok", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want exactly 1 (version check passed, launch proceeded)", exec.sessions)
	}
}

func TestDispatch_VersionCheckFails_NoTaskCreated(t *testing.T) {
	store, exec, r, model := setupWithVersionCheck(t, router.VersionCheck{
		Command: "claude --version",
		Parse:   router.ExtractDottedVersion,
		Min:     "9.9.9", // installed version will always be below this
	})
	ws := createFixtureWorkspace(t, store)

	exec.runOnceOutput = "2.1.251 (Claude Code)"

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "hi")
	if err == nil {
		t.Fatal("Dispatch with a below-minimum agent version: want error, got nil")
	}
	if len(exec.sessions) != 0 {
		t.Fatalf("sessions = %+v, want 0 — a failing version check must never create a session/task", exec.sessions)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("ListTasksByWorkspace = %+v, want 0 — no task record for a doomed launch", tasks)
	}
}

func TestDispatch_VersionCheckCommandFails_NoTaskCreated(t *testing.T) {
	store, exec, r, model := setupWithVersionCheck(t, router.VersionCheck{
		Command: "claude --version",
		Parse:   router.ExtractDottedVersion,
		Min:     "2.0.0",
	})
	ws := createFixtureWorkspace(t, store)

	exec.runOnceErr = errors.New("command not found")

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "hi")
	if err == nil {
		t.Fatal("Dispatch when the version-check command itself fails: want error, got nil")
	}
	if len(exec.sessions) != 0 {
		t.Fatalf("sessions = %+v, want 0", exec.sessions)
	}
}

func TestDispatch_NoVersionCheck_NeverCallsRunOnce(t *testing.T) {
	store, exec, r, model := setup(t) // the plain (no VersionCheck) fixture from dispatch_test.go
	ws := createFixtureWorkspace(t, store)

	// If RunOnce were ever called despite no VersionCheck being set, this
	// would make the launch fail — proving it's never invoked when there's
	// nothing to check.
	exec.runOnceErr = errors.New("RunOnce should never be called here")

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "ok", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
}

// TestDispatch_VersionTooOld_NamesRequirement: a CLI too old for what
// Loomux injects at launch (LOOM-75's completion hook) fails fast with a
// message saying what it's too old for, not just a bare range error.
func TestDispatch_VersionTooOld_NamesRequirement(t *testing.T) {
	store, exec, r, model := setupWithVersionCheck(t, router.VersionCheck{
		Command:  "claude --version",
		Parse:    router.ExtractDottedVersion,
		Min:      "9.9.9",
		Requires: "completion hooks (--settings)",
	})
	ws := createFixtureWorkspace(t, store)
	exec.runOnceOutput = "2.1.251 (Claude Code)"
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "hi")
	if err == nil {
		t.Fatal("Dispatch: want error, got nil")
	}
	want := "agent version too old for completion hooks (--settings)"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
	if !errors.Is(err, router.ErrVersionTooOld) {
		t.Fatalf("error = %v, want errors.Is(err, router.ErrVersionTooOld)", err)
	}
}
