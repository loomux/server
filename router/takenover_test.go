package router_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-266 (TUI S5): taking over a running turn ends the dispatch's
// wait at once. The dispatch fails with error class taken_over, no
// interrupt keys are sent, the task stays human_takeover, and what the
// agent finishes reaches the conversation later as late output
// (docs/design/tui-client-design.md, "Taking over a running turn").

// slowAgent's turns don't finish by themselves within a test: its pane
// never sits still for its idle timeout. It has interrupt keys, so a
// turn that wrongly interrupted it would show.
func slowAgent() router.AgentType {
	return router.AgentType{
		AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: time.Hour},
		LaunchTemplate: "claude",
		InterruptKeys:  []string{"Escape"},
	}
}

// takeOverMidTurn takes the task over from a terminal shortly after its
// turn's wait starts, as a person attaching to a busy agent would, and
// returns where the takeover's result arrives.
func (h *casHarness) takeOverMidTurn(t *testing.T) <-chan *orchestrator.TakeoverResult {
	t.Helper()
	taken := make(chan *orchestrator.TakeoverResult, 1)
	h.detector.onWait = func(task *registry.Task) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			res, err := h.orch.Takeover(context.Background(), orchestrator.TakeoverRequest{
				TaskID: task.ID, SessionID: "sess-tui", ClientKind: "tui", ClientLabel: "laptop"})
			if err != nil {
				t.Errorf("Takeover: %v", err)
			}
			taken <- res
		}()
	}
	return taken
}

// requireTakenOverErr: the turn's error says taken over, and a dispatch
// job records it as taken_over.
func requireTakenOverErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, orchestrator.ErrTakenOver) {
		t.Fatalf("Dispatch err = %v, want ErrTakenOver", err)
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassTakenOver {
		t.Fatalf("ClassifyError = %q, want taken_over", got)
	}
}

// The turn's wait ends when the task is taken over, not when the agent
// finishes (an hour away here): the dispatch returns at once, taken
// over, its task human_takeover under the new hold, nothing relayed and
// no key sent to the agent.
func TestDispatch_TakeoverEndsTheRunningTurnAtOnce(t *testing.T) {
	h := newCASHarness(t, slowAgent())
	taken := h.takeOverMidTurn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := h.r.Dispatch(ctx, "conv-1", "refactor the parser")
	if elapsed := time.Since(start); elapsed > 10*time.Second || ctx.Err() != nil {
		t.Fatalf("Dispatch returned after %s: the takeover didn't end the turn's wait", elapsed)
	}
	requireTakenOverErr(t, err)
	res := <-taken
	task := h.requireLeftTakenOver(t)
	if hold, herr := h.store.GetActiveDriveHold(context.Background(), task.ID); herr != nil || res == nil || hold.ID != res.Hold.ID {
		t.Fatalf("active hold = %+v, %v; want the takeover's", hold, herr)
	}
	if n := h.relayed.Load(); n != 0 {
		t.Fatalf("the turn was relayed %d times after its task was taken over", n)
	}
}

// Through a dispatch job, as the API runs it: the job ends failed with
// error class taken_over and an error a person can read, and the
// conversation is free again.
func TestDispatchJob_TakenOverTurnEndsAsTakenOver(t *testing.T) {
	h := newCASHarness(t, slowAgent())
	taken := h.takeOverMidTurn(t)
	svc := dispatch.New(h.store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		return h.r.Dispatch(ctx, d.ConversationID, d.Message)
	}, dispatch.WithErrorClassifier(router.ClassifyError))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
	})

	ctx := context.Background()
	d, err := svc.Submit(ctx, dispatch.Request{ConversationID: "conv-1", Message: "refactor the parser"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done, err := svc.Wait(waitCtx, d.ID)
	if err != nil {
		t.Fatalf("Wait: %v (the takeover didn't end the dispatch)", err)
	}
	<-taken
	if done.Status != registry.DispatchStatusFailed || done.ErrorClass != registry.ErrorClassTakenOver {
		t.Fatalf("dispatch = %s/%s (%q), want failed/taken_over", done.Status, done.ErrorClass, done.Error)
	}
	if !strings.Contains(done.Error, "taken over") || strings.Contains(done.Error, "orchestrator:") {
		t.Fatalf("dispatch error = %q, want a plain sentence saying it was taken over", done.Error)
	}
	h.requireLeftTakenOver(t)
	all, err := svc.ListByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range all {
		if !d.Status.Terminal() {
			t.Fatalf("dispatch %s is still %s: the conversation stays busy", d.ID, d.Status)
		}
	}
}

// A takeover that lands as the turn's wait finds something (the agent
// finished, timed out or stopped at a prompt) ends it as taken_over too,
// whichever way the turn was going to end.
func TestDispatch_TakeoverAsTheWaitEndsIsTakenOver(t *testing.T) {
	cases := []struct {
		name    string
		agent   router.AgentType
		capture string
	}{
		{"finished", router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond},
			LaunchTemplate: "claude"}, ""},
		{"timed out", router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, NoProgressTimeout: -1},
			LaunchTemplate: "claude", InterruptKeys: []string{"Escape"}}, ""},
		{"login prompt", promptAgent(map[string]*registry.Attention{
			"LOGIN": {Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: "Select login method:"},
		}), "LOGIN"},
		{"permission prompt", promptAgent(map[string]*registry.Attention{
			permissionScreen: {Kind: registry.AttentionPermission, Title: "Bash command", Question: "Do you want to proceed?",
				Options: []registry.AttentionOption{{Label: "Yes"}, {Label: "No"}}},
		}), permissionScreen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCASHarness(t, c.agent)
			h.exec.capture = c.capture
			h.takeOverDuringTheWait(t)

			_, err := h.r.Dispatch(context.Background(), "conv-1", "hello")
			requireTakenOverErr(t, err)
			h.requireLeftTakenOver(t)
		})
	}
}

// What the agent finishes after its turn was taken over isn't lost: once
// the task is handed back, the late-output pass relays it to the
// conversation (LOOM-121's pass, which already covers a task handed back
// from a takeover). While the task is held, nothing is relayed.
func TestTakenOverTurn_FinishIsRelayedAfterRelease(t *testing.T) {
	h := newLastMessageHarness(t)
	h.exec.setFileExists(false) // the agent is still working: no marker yet
	ctx := context.Background()

	type result struct{ err error }
	dispatched := make(chan result, 1)
	go func() {
		_, err := h.r.Dispatch(ctx, "conv-1", "run the migration")
		dispatched <- result{err}
	}()
	task := waitForTaskStatus(t, h.store, h.ws.ID, registry.TaskStatusRunning)
	res, err := h.orch.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-tui", ClientLabel: "laptop"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	select {
	case r := <-dispatched:
		requireTakenOverErr(t, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatch didn't end when its task was taken over")
	}

	// The agent finishes the turn while the person watches.
	late := "Migration done: 3 tables altered."
	h.mu.Lock()
	h.payload = hookPayload(t, "last_assistant_message", late)
	h.mu.Unlock()
	h.exec.setFileExists(true)
	if n := h.r.RelayLateOutput(ctx); n != 0 {
		t.Fatalf("RelayLateOutput while held = %d, want 0", n)
	}

	if _, err := h.orch.Release(ctx, task.ID, res.Hold.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n := h.r.RelayLateOutput(ctx); n != 1 {
		t.Fatalf("RelayLateOutput after release = %d, want 1", n)
	}
	h.mu.Lock()
	relayed := append([]string(nil), h.relayed...)
	h.mu.Unlock()
	if len(relayed) != 1 || relayed[0] != late {
		t.Fatalf("relayed %q, want only the agent's finish", relayed)
	}
	if msgs := assistantMessages(t, h.store, "conv-1"); len(msgs) != 1 || msgs[0].TaskID != task.ID {
		t.Fatalf("assistant messages = %+v, want the late reply for the task", msgs)
	}
}

// waitForTaskStatus waits for the workspace's one task to reach status.
func waitForTaskStatus(t *testing.T, store registry.Store, workspaceID string, status registry.TaskStatus) *registry.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := store.ListTasksByWorkspace(context.Background(), workspaceID)
		if err == nil && len(tasks) == 1 && tasks[0].Status == status {
			return tasks[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no task reached %s", status)
	return nil
}

// A status change that isn't a takeover (a cancel, say) keeps its own
// class: only a takeover is taken_over.
func TestClassifyError_TakenOver(t *testing.T) {
	changed := fmt.Errorf("router: dispatch: %w", orchestrator.ErrStatusChanged)
	if got := router.ClassifyError(changed); got == registry.ErrorClassTakenOver {
		t.Fatalf("ClassifyError(a status change) = %q, want not taken_over", got)
	}
	takenOver := fmt.Errorf("router: dispatch: %w: %w", orchestrator.ErrTakenOver, orchestrator.ErrStatusChanged)
	if got := router.ClassifyError(takenOver); got != registry.ErrorClassTakenOver {
		t.Fatalf("ClassifyError(taken over) = %q, want taken_over", got)
	}
}

// Review (LOOM-266, round 1): the prompt's answer is being typed when the
// task is taken over, and the key fails (the person's client is in the
// pane). The answer's turn ends taken over, not as a send failure, and
// the task is left to the person.
func TestDispatch_TakeoverWhileAnsweringAPromptIsTakenOver(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	task := stopAtPrompt(t, store, r, ws)
	exec.set(func() {
		exec.onSendKey = func() error {
			cur, err := store.GetTask(context.Background(), task.ID)
			if err != nil {
				return err
			}
			cur.Status = registry.TaskStatusHumanTakeover
			if err := store.UpdateTask(context.Background(), cur); err != nil {
				return err
			}
			return errors.New("tmux: send-keys failed")
		}
	})

	_, err := r.Dispatch(context.Background(), "conv-1", "approve")
	requireTakenOverErr(t, err)
	if got, _ := store.GetTask(context.Background(), task.ID); got.Status != registry.TaskStatusHumanTakeover || got.ErrorClass != "" {
		t.Fatalf("task = %s (class %q), want human_takeover, not failed", got.Status, got.ErrorClass)
	}
}
