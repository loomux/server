package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// markerTierAgentTypes mirrors app.DefaultAgentTypes's TierMarker
// declarations for "claude-code" and "codex" (both real production
// names, so this test exercises LOOM-32's "wire it through for both
// adapters" requirement literally) but swaps each LaunchTemplate for a
// fixed literal — this test only cares about what env vars Router
// injects around whatever template is configured, not about a real CLI.
func markerTierAgentTypes() router.AgentTypeRegistry {
	return router.AgentTypeRegistry{
		"":            router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle}},
		"claude-code": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierMarker}, LaunchTemplate: "claude"},
		"codex":       router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierMarker}, LaunchTemplate: "codex"},
	}
}

// TestDispatch_TierMarker_InjectsTaskIDAndMarkerPathEnv proves that
// launching a TierMarker-configured agent-type embeds LOOMUX_TASK_ID and
// LOOMUX_MARKER_PATH into the launched session's command, with values
// that actually match the real task that was created and the exact path
// completion.MarkerWatcher polls for (LOOM-32) — for both registered
// agent-type names, since the mechanism must generalize across adapters
// the way LOOM-22 proved the registry itself does.
func TestDispatch_TierMarker_InjectsTaskIDAndMarkerPathEnv(t *testing.T) {
	for _, agentType := range []string{"claude-code", "codex"} {
		t.Run(agentType, func(t *testing.T) {
			store := newTestStore(t)
			ws := createFixtureWorkspace(t, store)
			exec := newFakeExecutor()
			exec.fileExists = true // simulate the hook having already touched the marker
			markerDir := t.TempDir()
			agentTypes := markerTierAgentTypes()
			detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
			orch := orchestrator.New(store, exec.factory(), detector)
			resolver := credentials.NewResolver(store)
			model := &routertest.StubRoutingModel{
				DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
					return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: agentType}, nil
				},
				RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
					return router.RelayResult{Reply: "done", Done: true}, nil
				},
			}
			r := router.New(store, orch, exec.factory(), resolver, agentTypes, model, markerDir)

			if _, err := r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}

			tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
			if err != nil {
				t.Fatalf("ListTasksByWorkspace: %v", err)
			}
			if len(tasks) != 1 {
				t.Fatalf("tasks = %+v, want exactly 1", tasks)
			}
			task := tasks[0]

			var sess *fakeSession
			for _, s := range exec.sessions {
				sess = s
			}
			if sess == nil {
				t.Fatalf("no session launched")
			}

			wantTaskIDEnv := "LOOMUX_TASK_ID='" + task.ID + "'"
			if !strings.Contains(sess.command, wantTaskIDEnv) {
				t.Fatalf("session command = %q, want it to contain %q", sess.command, wantTaskIDEnv)
			}

			wantMarkerPath := completion.MarkerPath(markerDir, task.ID)
			wantMarkerEnv := "LOOMUX_MARKER_PATH='" + wantMarkerPath + "'"
			if !strings.Contains(sess.command, wantMarkerEnv) {
				t.Fatalf("session command = %q, want it to contain %q", sess.command, wantMarkerEnv)
			}

			if !strings.Contains(sess.command, agentTypes[agentType].LaunchTemplate) {
				t.Fatalf("session command = %q, want it to still contain the resolved launch template %q", sess.command, agentTypes[agentType].LaunchTemplate)
			}
		})
	}
}

// TestDispatch_TierIdle_DoesNotInjectMarkerPathEnv proves
// LOOMUX_MARKER_PATH is only injected for a TierMarker-configured
// agent-type — an idle-tier agent has no marker to write, so leaking an
// unused env var would be pure noise. LOOMUX_TASK_ID is still injected
// (generically useful, and cheap regardless of tier).
func TestDispatch_TierIdle_DoesNotInjectMarkerPathEnv(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "done", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want exactly 1", tasks)
	}
	task := tasks[0]

	var sess *fakeSession
	for _, s := range exec.sessions {
		sess = s
	}
	if sess == nil {
		t.Fatalf("no session launched")
	}

	wantTaskIDEnv := "LOOMUX_TASK_ID='" + task.ID + "'"
	if !strings.Contains(sess.command, wantTaskIDEnv) {
		t.Fatalf("session command = %q, want it to contain %q", sess.command, wantTaskIDEnv)
	}
	if strings.Contains(sess.command, "LOOMUX_MARKER_PATH") {
		t.Fatalf("session command = %q, want no LOOMUX_MARKER_PATH for a TierIdle agent-type", sess.command)
	}
}

// TestLaunchAgent_MintsDistinctTaskIDsAcrossLaunches guards against a
// naive implementation that computes the marker path once and reuses a
// stale task ID across launches — every real launch must get its own
// fresh ID.
func TestLaunchAgent_MintsDistinctTaskIDsAcrossLaunches(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	exec.fileExists = true
	markerDir := t.TempDir()
	agentTypes := markerTierAgentTypes()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "done", Done: true}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), resolver, agentTypes, model, markerDir)

	// Two separate conversations against the same workspace — each gets
	// its own fresh task (no active-task reuse across conversations), so
	// this proves launchAgent doesn't accidentally reuse or compute a
	// stale task ID across launches.
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch (conv-1): %v", err)
	}

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-2", "go"); err != nil {
		t.Fatalf("Dispatch (conv-2): %v", err)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want exactly 2", tasks)
	}
	if tasks[0].ID == tasks[1].ID {
		t.Fatalf("both launches got the same task ID %q, want distinct IDs", tasks[0].ID)
	}
}
