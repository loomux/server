package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// setRelayPolicy sets ws's target's purpose and relay policy.
func setRelayPolicy(t *testing.T, store registry.Store, ws *registry.Workspace, purpose, relay string) {
	t.Helper()
	ctx := context.Background()
	target, err := store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	target.Policy.Purpose, target.Policy.Relay = purpose, relay
	if err := store.UpdateTarget(ctx, target); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
}

var relayPolicyCases = []struct {
	name, purpose, relay string
	effective            string
}{
	{"full", "", registry.RelayFull, registry.RelayFull},
	{"last_message", "", registry.RelayLastMessage, registry.RelayLastMessage},
	{"none", "", registry.RelayNone, registry.RelayNone},
	{"work machine default", registry.TargetPurposeWork, "", registry.RelayNone},
	{"work machine set to full", registry.TargetPurposeWork, registry.RelayFull, registry.RelayFull},
}

// User decision 2026-10-07: a target's relay policy decides what the
// relay model gets of its turns: everything (full), only the agent's
// final message (last_message), or nothing (none: the agent's own words
// are the reply, and its session stays open).
func TestRelayPolicy_Turn(t *testing.T) {
	for _, tc := range relayPolicyCases {
		t.Run(tc.name, func(t *testing.T) {
			store, exec, r, model := setup(t)
			ws := createFixtureWorkspace(t, store)
			setRelayPolicy(t, store, ws, tc.purpose, tc.relay)
			ctx := context.Background()
			if err := store.SetWorkspaceRollingSummary(ctx, ws.ID, "earlier summary"); err != nil {
				t.Fatalf("SetWorkspaceRollingSummary: %v", err)
			}
			exec.capture = "working…\nAGENT-FINAL-ANSWER: the build passes"
			model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
				return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
			}
			var calls []router.RelayInput
			model.RelayInputFunc = func(ctx context.Context, in router.RelayInput) (router.RelayResult, error) {
				calls = append(calls, in)
				return router.RelayResult{Reply: "condensed", Done: true}, nil
			}

			reply, err := r.Dispatch(ctx, "conv-1", "does the build pass?")
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			switch tc.effective {
			case registry.RelayFull:
				if len(calls) != 1 || calls[0].UserMessage != "does the build pass?" || calls[0].PreviousSummary != "earlier summary" ||
					!strings.Contains(calls[0].Captured, "AGENT-FINAL-ANSWER") {
					t.Fatalf("relay input = %+v, want the message, summary and output", calls)
				}
			case registry.RelayLastMessage:
				if len(calls) != 1 || calls[0].UserMessage != "" || calls[0].PreviousSummary != "" ||
					!strings.Contains(calls[0].Captured, "AGENT-FINAL-ANSWER") {
					t.Fatalf("relay input = %+v, want only the agent's final message", calls)
				}
			case registry.RelayNone:
				if len(calls) != 0 {
					t.Fatalf("relay model was called %d time(s) under none: %+v", len(calls), calls)
				}
				if !strings.Contains(reply, "AGENT-FINAL-ANSWER: the build passes") {
					t.Errorf("reply = %q, want the agent's own words", reply)
				}
				tasks, _ := store.ListTasksByWorkspace(ctx, ws.ID)
				if len(tasks) != 1 || tasks[0].Status != registry.TaskStatusAwaitingInput {
					t.Errorf("tasks = %+v, want the session kept open (no model judged it done)", tasks)
				}
			}
		})
	}
}

// The same policy gates what the routing model sees of the target's
// conversations: its history (none: nothing; last_message: the replies
// only), the open task's last reply, and the workspace's summary.
func TestRelayPolicy_RoutingView(t *testing.T) {
	for _, tc := range relayPolicyCases {
		t.Run(tc.name, func(t *testing.T) {
			store, exec, r, model := setup(t)
			ws := createFixtureWorkspace(t, store)
			setRelayPolicy(t, store, ws, tc.purpose, tc.relay)
			ctx := context.Background()
			exec.capture = "AGENT-WORDS about the secret project"
			var seen []router.WorkspaceSnapshot
			model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
				seen = workspaces
				return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
			}
			model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
				return router.RelayResult{Reply: "REPLY-TEXT", Done: false}, nil
			}
			if _, err := r.Dispatch(ctx, "conv-1", "USER-QUESTION one"); err != nil {
				t.Fatalf("Dispatch 1: %v", err)
			}
			if _, err := r.Dispatch(ctx, "conv-1", "and then?"); err != nil {
				t.Fatalf("Dispatch 2: %v", err)
			}

			opts := model.LastDecideOptions
			var history []string
			for _, turn := range opts.History {
				history = append(history, turn.Role+":"+turn.Content)
			}
			h := strings.Join(history, " | ")
			var summary, lastReply string
			for _, s := range seen {
				if s.ID == ws.ID {
					summary = s.Summary
				}
			}
			if opts.OpenTask != nil {
				lastReply = opts.OpenTask.LastReply
			}
			switch tc.effective {
			case registry.RelayFull:
				if !strings.Contains(h, "USER-QUESTION") || !strings.Contains(h, "REPLY-TEXT") || summary == "" || lastReply == "" {
					t.Fatalf("full: history %q, summary %q, last reply %q; want all of it", h, summary, lastReply)
				}
			case registry.RelayLastMessage:
				if strings.Contains(h, "USER-QUESTION") || !strings.Contains(h, "REPLY-TEXT") || summary == "" {
					t.Fatalf("last_message: history %q, summary %q; want the replies only", h, summary)
				}
			case registry.RelayNone:
				if h != "" || summary != "" || lastReply != "" {
					t.Fatalf("none: history %q, summary %q, last reply %q; want nothing from the target", h, summary, lastReply)
				}
			}
		})
	}
}

// Messages logged before the policy existed carry no target: they are
// matched to it through their task, or failing that their purpose, so an
// old turn on a work machine stays out of the routing model's view too.
func TestRelayPolicy_OlderMessagesWithoutTarget(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	setRelayPolicy(t, store, ws, registry.TargetPurposeWork, "")
	ctx := context.Background()
	exec.capture = "AGENT-WORDS"
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	// A turn on the work machine's task, logged the old way, and one
	// known only by its purpose.
	if _, err := r.Dispatch(ctx, "conv-old-seed", "seed"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	task := &registry.Task{ID: "old-task", WorkspaceID: ws.ID, ConversationID: "conv-old", Kind: registry.TaskKindAgent,
		AgentType: "claude-code", Status: registry.TaskStatusCompleted, TmuxSession: "loomux-old"}
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, m := range []*registry.Message{
		{ID: "o1", ConversationID: "conv-old", TaskID: "old-task", Role: registry.MessageRoleUser, Content: "OLD-TASK-QUESTION"},
		{ID: "o2", ConversationID: "conv-old", TaskID: "old-task", Role: registry.MessageRoleAssistant, Content: "OLD-TASK-REPLY"},
		{ID: "o3", ConversationID: "conv-old", Origin: registry.TargetPurposeWork, Role: registry.MessageRoleAssistant, Content: "OLD-WORK-OFFER"},
		{ID: "o4", ConversationID: "conv-old", Origin: registry.MessageOriginNone, Role: registry.MessageRoleAssistant, Content: "ROUTER-OWN-ANSWER"},
	} {
		if err := store.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage %s: %v", m.ID, err)
		}
	}
	if _, err := r.Dispatch(ctx, "conv-old", "next"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var history []string
	for _, turn := range model.LastDecideOptions.History {
		history = append(history, turn.Content)
	}
	h := strings.Join(history, " | ")
	if strings.Contains(h, "OLD-") || !strings.Contains(h, "ROUTER-OWN-ANSWER") {
		t.Fatalf("history = %q, want the work machine's old turns left out and the router's own answer kept", h)
	}
}
