package router_test

import (
	"context"
	"fmt"
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

// LOOM-109: an agent that keeps compacting its context within a turn has
// the turn failed as compaction_loop, the agent interrupted and its pane
// kept, and the dispatch job classed the same.
func TestDispatch_CompactionLoop(t *testing.T) {
	store := newTestStore(t)
	exec := newFakeExecutor()
	n := 0
	exec.captureFunc = func() string {
		n++
		if n%2 == 0 {
			return "compacting"
		}
		return fmt.Sprintf("working %d", n)
	}
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{
			AgentConfig: completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: time.Minute, NoProgressTimeout: time.Minute,
				DetectCompaction: func(s string) bool { return s == "compacting" }},
			LaunchTemplate: "claude",
			InterruptKeys:  []string{"Escape"},
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir,
		completion.WithProgressPollInterval(5*time.Millisecond))
	orch := orchestrator.New(store, exec.factory(), detector)
	ws := createFixtureWorkspace(t, store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)

	_, err := r.Dispatch(context.Background(), "conv-1", "refactor everything")
	if err == nil {
		t.Fatal("Dispatch: nil error for a compaction loop")
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassCompactionLoop {
		t.Errorf("ClassifyError = %q, want compaction_loop", got)
	}
	if !strings.Contains(err.Error(), "compacted its context 3 times") || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error = %v", err)
	}
	task := onlyTask(t, store, ws.ID)
	assertFailed(t, task, registry.ErrorClassCompactionLoop, "compacted its context")
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the pane was torn down; keep it to look at")
	}
}
