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

// sentToAgent is everything the launched agentType task (on onTarget, if
// given) was given: its launch command (a profile may pass the first turn
// there) and the keys typed into its pane.
func (h *availabilityHarness) sentToAgent(t *testing.T, agentType string, onTarget ...string) string {
	t.Helper()
	tasks, err := h.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, task := range tasks {
		if !hasAgentTask([]*registry.Task{task}, agentType) {
			continue
		}
		if len(onTarget) > 0 {
			ws, err := h.store.GetWorkspace(context.Background(), task.WorkspaceID)
			if err != nil || ws.TargetID != onTarget[0] {
				continue
			}
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

// otherTarget registers a second target, of the given purpose, with an
// idle workspace on it.
func (h *availabilityHarness) otherTarget(t *testing.T, name, purpose string) (*registry.Target, *registry.Workspace) {
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
	return target, ws
}

// jobTurn runs message as conv-1's next turn the way production does: a
// dispatch job stores the user's message at submit, with no task, and
// the router writes only the reply.
func (h *availabilityHarness) jobTurn(t *testing.T, message string, d router.Decision) {
	t.Helper()
	id := uuid.NewString()
	createDispatchRow(t, h.store, id, "conv-1", &registry.Message{
		ID: uuid.NewString(), ConversationID: "conv-1", Role: registry.MessageRoleUser, Content: message,
	})
	h.decide(d)
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", message,
		router.WithDispatchID(id), router.WithUserMessageLogged()); err != nil {
		t.Fatalf("Dispatch %q: %v", message, err)
	}
	// The job is over, so the conversation's next one may start.
	d2, err := h.store.GetDispatch(ctx, id)
	if err != nil {
		t.Fatalf("GetDispatch: %v", err)
	}
	d2.Status = registry.DispatchStatusSucceeded
	if err := h.store.TransitionDispatch(ctx, d2, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
}

func (h *availabilityHarness) setPurpose(t *testing.T, purpose string) {
	t.Helper()
	h.target.Policy.Purpose = purpose
	if err := h.store.UpdateTarget(context.Background(), h.target); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
}

// startHere provisions a fresh agent on h.target, leaving any open task.
func (h *availabilityHarness) startHere(t *testing.T, message string) string {
	t.Helper()
	d := h.provisionDecision("claude-code")
	d.LeaveOpenTask = true
	h.jobTurn(t, message, d)
	return h.sentToAgent(t, "claude-code", h.target.ID)
}

// Command-center decision (2026-10-05, option B): a new agent only hears
// about earlier turns that ran on targets of its own purpose, so work
// context (sc1) and personal context never reach each other's agents —
// the user's requests as well as the replies, and offers made about the
// other target too. Turns that touched no target (the router's own
// answers) are kept.
func TestDispatch_EarlierConversationStaysWithinPurpose(t *testing.T) {
	for _, tc := range []struct {
		name, agentPurpose, otherPurpose string
	}{
		{"work agent, personal turns", registry.TargetPurposeWork, registry.TargetPurposePersonal},
		{"personal agent, work turns", "", registry.TargetPurposeWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAvailabilityHarness(t)
			h.probes.install("claude")
			h.recordAgents(t, map[string]bool{"claude-code": true})
			h.setPurpose(t, tc.agentPurpose)
			other, ws := h.otherTarget(t, "other", tc.otherPurpose)

			h.jobTurn(t, "check the payroll export", router.Decision{
				Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"})
			h.jobTurn(t, "how much disk is free there", router.Decision{
				Action: router.ActionRunCommand, TargetID: other.ID, Command: "df -h /srv/payroll"})
			h.jobTurn(t, "set up a scratch project", router.Decision{
				Action: router.ActionAnswerDirectly, DirectAnswer: "Which project do you mean?", LeaveOpenTask: true})
			sent := h.startHere(t, "a new one, please")

			for _, other := range []string{"payroll"} {
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

// Where a turn ran is recorded when it's logged, so deleting the
// workspace (which unlinks its messages from their tasks) doesn't make
// its turns look like the router's own and cross over.
func TestDispatch_EarlierConversationDeletedWorkspaceStaysWithinPurpose(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	_, ws := h.otherTarget(t, "sc1", registry.TargetPurposeWork)
	h.jobTurn(t, "check the payroll export", router.Decision{
		Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"})
	if err := h.store.DeleteWorkspaceAndTasks(context.Background(), ws.ID); err != nil {
		t.Fatalf("DeleteWorkspaceAndTasks: %v", err)
	}

	if sent := h.startHere(t, "something at home now"); strings.Contains(sent, "payroll") {
		t.Errorf("the agent got %q, want the deleted work workspace's turn left out", sent)
	}
}

// Same purpose: the earlier agent turn is kept, the user's request and
// the reply alike. An empty purpose is personal.
func TestDispatch_EarlierConversationKeepsSamePurpose(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	_, ws := h.otherTarget(t, "other", registry.TargetPurposePersonal)
	h.jobTurn(t, "count the files in notes", router.Decision{
		Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"})

	sent := h.startHere(t, "now somewhere new")
	if !strings.Contains(sent, "count the files in notes") || !strings.Contains(sent, "relayed") {
		t.Errorf("the agent got %q, want the same-purpose turn kept", sent)
	}
}

// Messages from before origins were recorded can't be placed, so they
// are left out.
func TestDispatch_EarlierConversationLeavesOutUnknownOrigin(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	if err := h.store.CreateMessage(context.Background(), &registry.Message{
		ID: uuid.NewString(), ConversationID: "conv-1", Role: registry.MessageRoleUser, Content: "an old request",
	}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	if sent := h.startHere(t, "start fresh"); strings.Contains(sent, "an old request") {
		t.Errorf("the agent got %q, want a message of unknown origin left out", sent)
	}
}
