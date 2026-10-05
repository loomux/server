package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// sentToAgent is everything the launched agentType task was given: its
// launch command (a profile may pass the first turn there) and the keys
// typed into its pane.
func (h *availabilityHarness) sentToAgent(t *testing.T, agentType string) string {
	t.Helper()
	tasks, err := h.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, task := range tasks {
		if !hasAgentTask([]*registry.Task{task}, agentType) {
			continue
		}
		// A task seeded straight into the store never ran here.
		if s := h.exec.sessionFor(task.TmuxSession); s != nil {
			return s.command + strings.Join(s.keys, "\n")
		}
	}
	t.Fatalf("no %s task in %+v", agentType, tasks)
	return ""
}

// Repeating an earlier message ("continue", "yes") is a new turn, not a
// confirmed request: everything before it is still earlier conversation.
func TestDispatch_RepeatedMessageKeepsEarlierConversation(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	ctx := context.Background()

	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Which repository should I look at?"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "review the open PRs"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "The loomux/server ones, then?"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "continue"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.decide(h.provisionDecision("claude-code"))
	if _, err := h.r.Dispatch(ctx, "conv-1", "continue"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	sent := h.sentToAgent(t, "claude-code")
	for _, want := range []string{"review the open PRs", "The loomux/server ones, then?"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the agent got %q, want the earlier conversation kept, with %q", sent, want)
		}
	}
}

// seedTurnOn records a finished agent turn of conv-1 on a new target of
// the given purpose: the user's message and the agent's reply.
func (h *availabilityHarness) seedTurnOn(t *testing.T, name, purpose, user, reply string) {
	t.Helper()
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: name, Kind: registry.TargetKindLocal,
		Policy: registry.TargetPolicy{Purpose: purpose}}
	if err := h.store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: name + "-ws", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-" + name, Status: registry.TaskStatusCompleted, ConversationID: "conv-1"}
	if err := h.store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, m := range []*registry.Message{
		{ID: uuid.NewString(), ConversationID: "conv-1", TaskID: task.ID, Role: registry.MessageRoleUser, Content: user},
		{ID: uuid.NewString(), ConversationID: "conv-1", TaskID: task.ID, Role: registry.MessageRoleAssistant, Content: reply},
	} {
		if err := h.store.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
	}
}

func (h *availabilityHarness) setPurpose(t *testing.T, purpose string) {
	t.Helper()
	h.target.Policy.Purpose = purpose
	if err := h.store.UpdateTarget(context.Background(), h.target); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
}

// Command-center decision (2026-10-05, option B): a new agent only hears
// about earlier turns that ran on targets of its own purpose, so work
// context (sc1) and personal context never reach each other's agents.
// Turns that touched no target (the router's own answers) are kept.
func TestDispatch_EarlierConversationStaysWithinPurpose(t *testing.T) {
	for _, tc := range []struct {
		name, agentPurpose, otherPurpose string
	}{
		{"work agent, personal turn", registry.TargetPurposeWork, registry.TargetPurposePersonal},
		{"personal agent, work turn", "", registry.TargetPurposeWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAvailabilityHarness(t)
			h.probes.install("claude")
			h.recordAgents(t, map[string]bool{"claude-code": true})
			h.setPurpose(t, tc.agentPurpose)
			ctx := context.Background()

			h.seedTurnOn(t, "other", tc.otherPurpose, "check the payroll export", "the export has 42 rows")
			h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Which project do you mean?"})
			if _, err := h.r.Dispatch(ctx, "conv-1", "set up a scratch project"); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			h.decide(h.provisionDecision("claude-code"))
			if _, err := h.r.Dispatch(ctx, "conv-1", "a new one, please"); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}

			sent := h.sentToAgent(t, "claude-code")
			for _, other := range []string{"payroll", "42 rows"} {
				if strings.Contains(sent, other) {
					t.Errorf("the agent got %q, want nothing from a target of another purpose", sent)
				}
			}
			if !strings.Contains(sent, "set up a scratch project") || !strings.Contains(sent, "Which project do you mean?") {
				t.Errorf("the agent got %q, want the router-only turns kept", sent)
			}
		})
	}
}

// Same purpose: the earlier agent turn is kept.
func TestDispatch_EarlierConversationKeepsSamePurpose(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	h.seedTurnOn(t, "other", registry.TargetPurposePersonal, "count the files in notes", "there are 7 files")
	h.decide(h.provisionDecision("claude-code"))
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "now somewhere new"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if sent := h.sentToAgent(t, "claude-code"); !strings.Contains(sent, "there are 7 files") {
		t.Errorf("the agent got %q, want the same-purpose turn kept (empty purpose is personal)", sent)
	}
}
