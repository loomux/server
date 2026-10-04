package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-87: the routing model sees the conversation so far — not the
// message being routed, which a dispatch job (LOOM-80) has already
// stored.
func TestDispatch_HistoryPassedToDecide(t *testing.T) {
	store, _, r, model := setup(t)
	answer := "first answer"
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: answer}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-h", "first question"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	if h := model.LastDecideOptions.History; len(h) != 0 {
		t.Fatalf("first turn's history = %+v, want none", h)
	}

	createDispatchRow(t, store, "d-h", "conv-h", &registry.Message{
		ID: uuid.NewString(), ConversationID: "conv-h", Role: registry.MessageRoleUser, Content: "second question",
	})
	answer = "second answer"
	if _, err := r.Dispatch(context.Background(), "conv-h", "second question",
		router.WithDispatchID("d-h"), router.WithUserMessageLogged()); err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	got := model.LastDecideOptions.History
	want := []router.ConversationTurn{{Role: "user", Content: "first question"}, {Role: "assistant", Content: "first answer"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// LOOM-87: history is the last few turns, each cut — a user message
// keeps its start, a reply its end, where a question usually is — and
// the block as a whole is bounded.
func TestDispatch_HistoryBounded(t *testing.T) {
	store, _, r, model := setup(t)
	ctx := context.Background()
	for i := range 10 {
		role := registry.MessageRoleUser
		content := "U" + strings.Repeat("u", 900)
		if i%2 == 1 {
			role = registry.MessageRoleAssistant
			content = strings.Repeat("a", 900) + "Z?"
		}
		if err := store.CreateMessage(ctx, &registry.Message{ID: uuid.NewString(), ConversationID: "conv-b", Role: role, Content: content}); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(ctx, "conv-b", "next"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h := model.LastDecideOptions.History
	if len(h) == 0 || len(h) > router.HistoryMessages {
		t.Fatalf("history has %d turns, want 1..%d", len(h), router.HistoryMessages)
	}
	total := 0
	for _, turn := range h {
		n := len([]rune(turn.Content))
		total += n
		if n > router.HistoryMessageRunes+1 {
			t.Errorf("turn is %d runes, want ≤ %d+1", n, router.HistoryMessageRunes)
		}
		if turn.Role == "user" && !strings.HasPrefix(turn.Content, "U") {
			t.Errorf("user turn lost its start: %.20q…", turn.Content)
		}
		if turn.Role == "assistant" && !strings.HasSuffix(turn.Content, "Z?") {
			t.Errorf("assistant turn lost its end: …%q", turn.Content[len(turn.Content)-10:])
		}
	}
	if total > router.HistoryRunes+len(h) {
		t.Errorf("history totals %d runes, want ≤ %d", total, router.HistoryRunes)
	}
	if h[len(h)-1].Role != "assistant" {
		t.Errorf("history doesn't end with the latest turn: %+v", h[len(h)-1].Role)
	}
}

// openTaskHarness: an agent turn that ends asking a question, leaving
// the conversation's task awaiting input.
func openTaskHarness(t *testing.T) (*routerHarness, *registry.Task) {
	t.Helper()
	h := newRouterHarness(t)
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "I've drafted the migration. Should I also update the tests?", Done: false}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "write the migration"); err != nil {
		t.Fatalf("Dispatch (first turn): %v", err)
	}
	task := onlyTask(t, h.store, h.ws.ID)
	if task.Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task after a done=false turn = %s, want awaiting_input", task.Status)
	}
	return h, task
}

// LOOM-87: the open task — workspace, agent, status and what its agent
// last said — reaches the routing model.
func TestDispatch_OpenTaskPassedToDecide(t *testing.T) {
	h, task := openTaskHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "claude-code"}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	open := h.model.LastDecideOptions.OpenTask
	if open == nil {
		t.Fatal("no open task passed to Decide")
	}
	if open.TaskID != task.ID || open.WorkspaceID != h.ws.ID || open.WorkspaceName != h.ws.Name ||
		open.AgentType != "claude-code" || open.Status != string(registry.TaskStatusAwaitingInput) ||
		!strings.Contains(open.LastReply, "Should I also update the tests?") {
		t.Errorf("open task = %+v", open)
	}
}

// LOOM-87 acceptance: an agent's clarifying question answered with
// "yes" reaches the same pane — even when the model would have answered
// it itself.
func TestDispatch_AffinityOverride_YesReachesSamePane(t *testing.T) {
	h, task := openTaskHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Sure!"}, nil
	}
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "Tests updated.", Done: true}, nil
	}
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "Tests updated." {
		t.Errorf("reply = %q, want the agent's reply, not the router's own answer", reply)
	}
	keys := h.exec.sessionFor(task.TmuxSession).keys
	if len(keys) == 0 || keys[len(keys)-1] != "yes" {
		t.Errorf("keys typed into the open task's pane = %v, want it to end with \"yes\"", keys)
	}
	if tasks, _ := h.store.ListTasksByWorkspace(context.Background(), h.ws.ID); len(tasks) != 1 {
		t.Errorf("tasks = %+v, want the one open task reused", tasks)
	}
}

// The override is logged on the routing decision.
func TestDispatch_AffinityOverride_Logged(t *testing.T) {
	logs := &logBuffer{}
	store := newTestStore(t)
	_, r, model := newRouter(t, store, router.WithLogger(logs.logger()))
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "Which option?", Done: false}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "do it"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Option 2 is fine"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "option 2"); err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	var found bool
	for _, rec := range logs.records(t) {
		if rec["msg"] == "routing decision" && rec["action"] == "use_workspace" && rec["affinity_override_from"] == "answer_directly" {
			found = true
		}
	}
	if !found {
		t.Errorf("no routing decision with affinity_override_from=answer_directly:\n%s", logs.buf.String())
	}
}

// The model can leave the open task, but only by saying so.
func TestDispatch_LeaveOpenTask_Respected(t *testing.T) {
	h, task := openTaskHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "It's 9pm in Tokyo.", LeaveOpenTask: true}, nil
	}
	before := len(h.exec.sessionFor(task.TmuxSession).keys)
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "what time is it in Tokyo?")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "It's 9pm in Tokyo." {
		t.Errorf("reply = %q, want the direct answer", reply)
	}
	if after := len(h.exec.sessionFor(task.TmuxSession).keys); after != before {
		t.Errorf("message typed into the open task's pane despite leave_open_task")
	}
}

// Routing into the open task's workspace always uses its agent: the
// turn goes into the pane that's there.
func TestDispatch_AffinityNormalisesAgent(t *testing.T) {
	h, task := openTaskHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "codex"}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if tasks, _ := h.store.ListTasksByWorkspace(context.Background(), h.ws.ID); len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Errorf("tasks = %+v, want the open claude-code task reused", tasks)
	}
}

// A finished task isn't open; its workspace is passed as the
// conversation's last one, as soft context.
func TestDispatch_NoOpenTask_LastWorkspacePassed(t *testing.T) {
	h := newRouterHarness(t)
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "done", Done: true}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "do it"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "thanks"); err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	o := h.model.LastDecideOptions
	if o.OpenTask != nil {
		t.Errorf("a completed task was passed as open: %+v", o.OpenTask)
	}
	if o.LastWorkspaceID != h.ws.ID || o.LastWorkspaceName != h.ws.Name {
		t.Errorf("last workspace = %q/%q, want %s", o.LastWorkspaceID, o.LastWorkspaceName, h.ws.ID)
	}
}
