package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
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

// LOOM-112 review: the message and the previous summary cross the same
// boundary as the output, to the relay vendor, so a vault value in
// either is redacted on the way, as in the output.
func TestDispatch_RelayInputRedactsSecrets(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	ctx := context.Background()
	const inMessage, inSummary = "msg-secret-4f1c9a", "sum-secret-8b2e7d"
	for name, value := range map[string]string{"MSG_TOKEN": inMessage, "SUM_TOKEN": inSummary} {
		if err := store.CreateCredential(ctx, &registry.Credential{ID: uuid.NewString(), Name: name, Value: value}); err != nil {
			t.Fatalf("CreateCredential: %v", err)
		}
	}
	if err := store.SetWorkspaceRollingSummary(ctx, ws.ID, "Earlier the agent used "+inSummary+" to log in."); err != nil {
		t.Fatalf("SetWorkspaceRollingSummary: %v", err)
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	var got router.RelayInput
	model.RelayInputFunc = func(ctx context.Context, in router.RelayInput) (router.RelayResult, error) {
		got = in
		return router.RelayResult{Reply: "done", Done: true}, nil
	}

	if _, err := r.Dispatch(ctx, "conv-1", "use token "+inMessage+" for the deploy"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got.UserMessage == "" || got.PreviousSummary == "" {
		t.Fatalf("relay input = %+v, want both a message and a previous summary", got)
	}
	for _, field := range []string{got.UserMessage, got.PreviousSummary, got.Captured} {
		if strings.Contains(field, inMessage) || strings.Contains(field, inSummary) {
			t.Errorf("a vault value reached the relay model: %q", field)
		}
	}
	if !strings.Contains(got.UserMessage, "for the deploy") || !strings.Contains(got.PreviousSummary, "to log in") {
		t.Errorf("relay input = %+v, want the rest of the message and summary kept", got)
	}
}
