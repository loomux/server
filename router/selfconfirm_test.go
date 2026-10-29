package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func TestAsksForConfirmation(t *testing.T) {
	asks := []string{
		// F8 in the LOOM-234 readiness run, as the routing model wrote it.
		`Please confirm starting new work in api-server on jet01. Reply yes to go ahead.`,
		// Loomux's own offers, which it copied.
		`jet01 asks for confirmation before new work starts there. I would start codex.` + "\n\n" +
			`Reply "yes" to go ahead. Any other reply cancels.`,
		`I'd run this on jet01:` + "\n\n    uptime\n\n" + `Reply "yes" to run it. Any other reply cancels.`,
		"Confirm and I'll start.",
		"Can you confirm you want a new workspace?",
		"Once you confirm, I'll clone it.",
		`Just say "yes" and I'll start.`,
		"Respond with OK to proceed.",
	}
	for _, s := range asks {
		if !router.AsksForConfirmation(s) {
			t.Errorf("AsksForConfirmation(%q) = false, want true", s)
		}
	}
	plain := []string{
		"Workspace names use dashes. Try e2e-loom56?",
		"Confirmed: the build passed on jet01.",
		"I can't confirm the disk is healthy from here; ask me to run df on it.",
		"The answer is yes: Loomux can run codex there.",
		"Which workspace should it go in, api-server or billing?",
		// Loomux's own direct answers.
		router.NoTargetsReply,
		router.NoMachinePluginReply,
	}
	for _, s := range plain {
		if router.AsksForConfirmation(s) {
			t.Errorf("AsksForConfirmation(%q) = true, want false", s)
		}
	}
}

// F8 in the LOOM-234 readiness run: the routing model answered with
// Loomux's own policy-confirmation text, but no offer was made, so the
// user's "yes" approved nothing. The request for a yes is taken out of
// the answer, which says nothing is waiting for one; the yes that
// follows still starts nothing (the bare-affirmative guard, #478).
func TestDispatch_ModelAuthoredConfirmationIsTakenOut(t *testing.T) {
	h := newRouterHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "I'd start new work in api-server on jet01. " +
			"Please confirm starting new work there.\n\nReply \"yes\" to go ahead. Any other reply cancels."}, nil
	}
	ctx := context.Background()
	reply, err := h.r.Dispatch(ctx, "conv-1", "start new work in api-server: add a changelog")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if router.AsksForConfirmation(reply) {
		t.Errorf("reply = %q, still asks for a yes", reply)
	}
	if !strings.Contains(reply, "I'd start new work in api-server on jet01.") || !strings.Contains(reply, "nothing is waiting for a yes") {
		t.Errorf("reply = %q, want the answer, saying nothing is waiting for a yes", reply)
	}
	events, err := h.store.ListDispatchEventsByConversation(ctx, "conv-1")
	recorded := false
	for _, e := range events {
		recorded = recorded || e.Kind == registry.EventDecision && strings.Contains(e.Detail, "confirmation request taken out")
	}
	if err != nil || !recorded {
		t.Errorf("events = %+v, %v; want the override on the decision", events, err)
	}

	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "claude-code"}, nil
	}
	reply, err = h.r.Dispatch(ctx, "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	if !strings.Contains(reply, "haven't started anything") {
		t.Errorf("reply to yes = %q, want it asked about", reply)
	}
	assertNothingStarted(t, h, h.ws.ID)
}

// An answer made of nothing but the request gets the note alone.
func TestDispatch_ModelAuthoredConfirmationOnly(t *testing.T) {
	h := newRouterHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Please confirm. Reply yes."}, nil
	}
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "start new work")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.HasPrefix(reply, "Nothing has started") {
		t.Errorf("reply = %q, want only the note", reply)
	}
}

// A plain answer goes out as the model wrote it.
func TestDispatch_PlainAnswerUntouched(t *testing.T) {
	h := newRouterHarness(t)
	const answer = "Workspace names use dashes. Try e2e-loom56?"
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: answer}, nil
	}
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "make e2e_loom56")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != answer {
		t.Errorf("reply = %q, want %q", reply, answer)
	}
}
