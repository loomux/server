package router_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// TestIntegration_RealDispatch_TierMarker proves LOOM-32's fix actually
// works end-to-end, for both real registered agent-type names — not
// just that the env vars appear in a session's command string
// (dispatch_test.go's TestDispatch_TierMarker_InjectsTaskIDAndMarkerPathEnv
// already proves that against a fake executor), but that a process
// running inside a *real* local tmux session can read LOOMUX_TASK_ID and
// LOOMUX_MARKER_PATH from its own environment and touch the exact file a
// real completion.Detector is polling for, causing the real detector to
// actually fire.
//
// The LaunchTemplate here is a stub standing in for a Claude Code Stop
// hook / Codex notify script — a real one is the target machine's own
// CLI config (design spec §7's "Loomux doesn't push OAuth-CLI config"
// precedent), out of this repo's scope; what this test proves is that
// Loomux's own half of the contract (the env vars a hook would read) is
// real and correct.
func TestIntegration_RealDispatch_TierMarker(t *testing.T) {
	for _, agentType := range []string{"claude-code", "codex"} {
		t.Run(agentType, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()

			target := &registry.Target{ID: uuid.NewString(), Name: "real-target-" + t.Name(), Kind: registry.TargetKindLocal}
			if err := store.CreateTarget(ctx, target); err != nil {
				t.Fatalf("CreateTarget: %v", err)
			}
			ws := &registry.Workspace{
				ID:       uuid.NewString(),
				Name:     "real-ws-" + t.Name(),
				Path:     t.TempDir(),
				TargetID: target.ID,
				Status:   registry.WorkspaceStatusIdle,
			}
			if err := store.CreateWorkspace(ctx, ws); err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}

			// Stands in for a real Stop hook / notify script: echoes the
			// task ID it was told (so the test can assert it matches the
			// task Dispatch actually created), then touches the marker
			// path it was told, exactly as a real hook would.
			const stubHookScript = `sh -c 'echo "task-id-seen=$LOOMUX_TASK_ID"; touch "$LOOMUX_MARKER_PATH"; sleep 30'`

			agentTypes := router.AgentTypeRegistry{
				agentType: router.AgentType{
					AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker},
					LaunchTemplate: stubHookScript,
				},
			}
			markerDir := t.TempDir()
			detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
			orch := orchestrator.New(store, targets.NewExecutor, detector)
			resolver := credentials.NewResolver(store)
			model := &routertest.StubRoutingModel{
				DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
					return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: agentType}, nil
				},
				RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
					return router.RelayResult{Reply: "relayed: " + strings.TrimSpace(captured), Done: true}, nil
				},
			}

			r := router.New(store, orch, targets.NewExecutor, resolver, agentTypes, model, markerDir)

			// A hard deadline: if the env vars were ever wrong (e.g. the
			// hook can't find the marker path it was told), MarkerWatcher
			// polls forever with no idle fallback — this bounds the test
			// instead of hanging the suite.
			dispatchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			reply, err := r.Dispatch(dispatchCtx, "conv-real", "do the real thing")
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}

			tasks, err := store.ListTasksByWorkspace(ctx, ws.ID)
			if err != nil {
				t.Fatalf("ListTasksByWorkspace: %v", err)
			}
			if len(tasks) != 1 {
				t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task", tasks)
			}
			task := tasks[0]

			wantSeen := "task-id-seen=" + task.ID
			if !strings.Contains(reply, wantSeen) {
				t.Fatalf("reply = %q, want it to contain %q (the hook's own env-var readback)", reply, wantSeen)
			}

			if task.Status != registry.TaskStatusCompleted {
				t.Fatalf("task Status = %q, want %q (marker detection must have fired, not merely timed out)", task.Status, registry.TaskStatusCompleted)
			}

			realExec := targets.NewLocalExecutor()
			t.Cleanup(func() {
				if err := realExec.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			exists, err := realExec.HasSession(ctx, task.TmuxSession)
			if err != nil {
				t.Fatalf("HasSession: %v", err)
			}
			// Kept for the grace period after completing (LOOM-91).
			if !exists {
				t.Fatalf("real tmux session %q torn down at completion, want it kept for the grace period", task.TmuxSession)
			}
			t.Cleanup(func() { _ = realExec.KillSession(context.Background(), task.TmuxSession) })

			// MarkerWatcher removes the marker file (best-effort) once it
			// detects it — confirms the whole loop closed cleanly, not
			// just that *something* eventually happened.
			markerPath := completion.MarkerPath(markerDir, task.ID)
			markerExists, err := realExec.FileExists(ctx, markerPath)
			if err != nil {
				t.Fatalf("FileExists(%q): %v", markerPath, err)
			}
			if markerExists {
				t.Fatalf("marker file %q still exists after detection, want it removed", markerPath)
			}
		})
	}
}

// TestIntegration_RealDispatch_RelaysSavedLastMessage runs the LOOM-91
// path for real: the agent's hook saves its payload beside the marker
// (as agents' hooks do), and the relay gets the message from it — not
// the pane, which shows something else — with the payload removed after.
func TestIntegration_RealDispatch_RelaysSavedLastMessage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "real-target", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "real-ws", Path: t.TempDir(), TargetID: target.ID,
		Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	const hook = `sh -c 'echo on-screen-only; ` +
		`printf "%s" "{\"last_assistant_message\":\"the whole answer\"}" > "$LOOMUX_MARKER_PATH.reply"; ` +
		`touch "$LOOMUX_MARKER_PATH"; sleep 30'`
	agentTypes := router.AgentTypeRegistry{
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker},
			LaunchTemplate: hook, LastMessageKey: "last_assistant_message",
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	var relayed string
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			relayed = captured
			return router.RelayResult{Reply: "ok", Done: true}, nil
		},
	}
	r := router.New(store, orch, targets.NewExecutor, credentials.NewResolver(store), agentTypes, model, markerDir)
	dispatchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.Dispatch(dispatchCtx, "conv-real", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tasks, _ := store.ListTasksByWorkspace(ctx, ws.ID)
	if len(tasks) == 1 {
		t.Cleanup(func() { _ = targets.NewLocalExecutor().KillSession(context.Background(), tasks[0].TmuxSession) })
	}
	if relayed != "the whole answer" {
		t.Fatalf("relayed %q, want the saved last message", relayed)
	}
	matches, _ := filepath.Glob(filepath.Join(markerDir, "*.reply"))
	if len(matches) != 0 {
		t.Errorf("payload left behind: %v", matches)
	}
}
