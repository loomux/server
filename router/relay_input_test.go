package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

// LOOM-112: the relay model gets the turn's message, the workspace's
// summary from before the turn, and the agent type, besides the output.
func TestDispatch_RelayGetsTurnContext(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	var got []router.RelayInput
	model.RelayInputFunc = func(ctx context.Context, in router.RelayInput) (router.RelayResult, error) {
		got = append(got, in)
		return router.RelayResult{Reply: "summary after turn " + string(rune('0'+len(got))), Done: false}, nil
	}
	ctx := context.Background()

	if _, err := r.Dispatch(ctx, "conv-1", "first message"); err != nil {
		t.Fatalf("Dispatch (first turn): %v", err)
	}
	if _, err := r.Dispatch(ctx, "conv-1", "second message"); err != nil {
		t.Fatalf("Dispatch (second turn): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("relay calls = %d, want 2", len(got))
	}
	if !strings.Contains(got[0].UserMessage, "first message") || got[0].AgentType != "claude-code" {
		t.Errorf("first relay input = %+v, want the first message and agent type", got[0])
	}
	if got[1].UserMessage != "second message" || got[1].PreviousSummary != "summary after turn 1" {
		t.Errorf("second relay input = %+v, want the second message and the first turn's summary", got[1])
	}
}
