package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// Found in the LOOM-56 pass: the routing model asked for a valid
// workspace name, the user answered "Use e2e-loom56 then.", and the agent
// started there got only that answer, never the task it was for. An agent
// started mid-conversation is told what was said before it.
func TestDispatch_FreshAgentGetsEarlierConversation(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	ctx := context.Background()

	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Workspace names use dashes. Try e2e-loom56?"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "In a new workspace e2e_loom56, create hello.txt containing loomux"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.decide(h.provisionDecision("claude-code"))
	if _, err := h.r.Dispatch(ctx, "conv-1", "Use e2e-loom56 then."); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	sent := h.sentToAgent(t, "claude-code")
	for _, want := range []string{
		"create hello.txt containing loomux",
		"Workspace names use dashes",
		"Use e2e-loom56 then.",
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("the agent got %q, want it to include %q", sent, want)
		}
	}
	if strings.Index(sent, "create hello.txt") > strings.Index(sent, "Use e2e-loom56 then.") {
		t.Errorf("the agent got %q, want the earlier conversation before the message", sent)
	}
	// History keeps the user's own words.
	msgs, _ := h.store.ListMessagesByConversation(ctx, "conv-1")
	if len(msgs) != 4 || msgs[2].Content != "Use e2e-loom56 then." {
		t.Errorf("stored messages = %+v, want the user's message as typed", msgs)
	}
}

// The first message of a conversation has nothing before it: the agent
// gets the message alone, as before.
func TestDispatch_FirstMessageHasNoContextNote(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	h.decide(h.provisionDecision("claude-code"))

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "set up a scratch project"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if sent := h.sentToAgent(t, "claude-code"); strings.Contains(sent, "Loomux note") {
		t.Errorf("the agent got %q, want no note on a conversation's first message", sent)
	}
}

// sentToAgent is everything the conversation's agentType task was given:
// its launch command (a profile may pass the first turn there) and the
// keys typed into its pane.
func (h *availabilityHarness) sentToAgent(t *testing.T, agentType string) string {
	t.Helper()
	tasks, err := h.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, task := range tasks {
		if hasAgentTask([]*registry.Task{task}, agentType) {
			s := h.exec.sessionFor(task.TmuxSession)
			return s.command + strings.Join(s.keys, "\n")
		}
	}
	t.Fatalf("no %s task in %+v", agentType, tasks)
	return ""
}
