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

// LOOM-76 acceptance: an agent that never signals completion fails the
// turn after its configured bound, with error class "timeout" and a
// reason on the task, the workspace back to idle, and the pane left
// alive for a human to attach to — named in the error.
func TestDispatch_TurnTimeout(t *testing.T) {
	store := newTestStore(t)
	exec := newFakeExecutor()
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, NoProgressTimeout: -1},
			LaunchTemplate: "claude",
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	model := &routertest.StubRoutingModel{}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	start := time.Now()
	_, err := r.Dispatch(context.Background(), "conv-1", "do something long")
	if err == nil {
		t.Fatal("Dispatch: nil error for a turn that never completed")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Dispatch took %s; want it bounded by the 300ms max turn", time.Since(start))
	}
	var timeout *orchestrator.TurnTimeoutError
	if !errors.As(err, &timeout) {
		t.Errorf("error %v doesn't carry the TurnTimeoutError", err)
	}
	task := onlyTask(t, store, ws.ID)
	for _, want := range []string{"claude-code", "timed out", "tmux -L loomux attach -t " + task.TmuxSession} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	assertFailed(t, task, registry.ErrorClassTimeout, "timed out")
	gotWS, _ := store.GetWorkspace(context.Background(), ws.ID)
	if gotWS.Status != registry.WorkspaceStatusIdle {
		t.Errorf("workspace status = %q, want idle", gotWS.Status)
	}
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the timed-out task's pane was torn down; it must be left for inspection")
	}
}
