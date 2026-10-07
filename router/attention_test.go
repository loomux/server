package router_test

import (
	"context"
	"errors"
	"slices"
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

const permissionScreen = "PERMISSION PROMPT"

// attentionHarness: a claude-code (marker-tier) agent whose pane shows
// permissionScreen until a test changes exec.capture. Its prompt reads
// as a three-option permission; any other screen as no prompt.
func attentionHarness(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *registry.Workspace) {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	exec.capture = permissionScreen
	detect := func(screen string) *registry.Attention {
		switch screen {
		case permissionScreen:
			return &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command", Detail: "rm -rf build",
				Question: "Do you want to proceed?",
				Options:  []registry.AttentionOption{{Label: "Yes"}, {Label: "Yes, and don't ask again"}, {Label: "No"}}}
		case "LOGIN":
			return &registry.Attention{Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: "Select login method:"}
		case "LIMIT":
			return &registry.Attention{Kind: registry.AttentionUsageLimit, Title: "Usage limit reached",
				Detail: "5-hour limit reached ∙ resets 5pm (Europe/Istanbul)", Resets: "5pm (Europe/Istanbul)"}
		}
		return nil
	}
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{
			AgentConfig: completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: time.Minute,
				NoProgressTimeout: time.Minute, DetectPrompt: detect},
			LaunchTemplate: "claude",
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir,
		completion.WithProgressPollInterval(10*time.Millisecond))
	orch := orchestrator.New(store, exec.factory(), detector)
	ws := createFixtureWorkspace(t, store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "relayed: " + captured}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	return store, exec, r, ws
}

func (e *fakeExecutor) set(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f()
}

// stopAtPrompt dispatches a turn that stops at the permission prompt.
func stopAtPrompt(t *testing.T, store registry.Store, r *router.Router, ws *registry.Workspace) *registry.Task {
	t.Helper()
	start := time.Now()
	reply, err := r.Dispatch(context.Background(), "conv-1", "clean the build")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Dispatch took %s: the prompt should end the wait fast", time.Since(start))
	}
	for _, want := range []string{"claude-code", "needs your approval", "rm -rf build", "Do you want to proceed?", "3. No", `"approve"`} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	task := onlyTask(t, store, ws.ID)
	if task.Status != registry.TaskStatusNeedsAttention || task.Attention == nil || task.Attention.Title != "Bash command" {
		t.Fatalf("task = %s / %+v, want needs-attention with the prompt", task.Status, task.Attention)
	}
	return task
}

// LOOM-97: a permission prompt ends the turn as needs-attention, and
// "approve" chooses its Yes and carries the turn on to completion.
func TestDispatch_PromptApproved(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	task := stopAtPrompt(t, store, r, ws)

	exec.set(func() { exec.capture, exec.fileExists = "all cleaned", true })
	reply, err := r.Dispatch(context.Background(), "conv-1", "Approve")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if reply != "relayed: all cleaned" {
		t.Errorf("reply = %q, want the finished turn's relay", reply)
	}
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; !slices.Equal(keys, []string{"Enter"}) {
		t.Errorf("keys = %v, want Enter on the selected Yes", keys)
	}
	got, _ := store.GetTask(context.Background(), task.ID)
	if got.Status != registry.TaskStatusAwaitingInput || got.Attention != nil {
		t.Errorf("task = %s / %+v, want awaiting-input with the prompt cleared", got.Status, got.Attention)
	}
}

// "deny" picks No, which ends the agent's turn without a completion
// signal: the turn is over once the pane settles.
func TestDispatch_PromptDenied(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	task := stopAtPrompt(t, store, r, ws)

	exec.set(func() { exec.capture = "Interrupted · What should Claude do instead?" })
	reply, err := r.Dispatch(context.Background(), "conv-1", "deny")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(reply, "What should Claude do instead?") {
		t.Errorf("reply = %q", reply)
	}
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; !slices.Equal(keys, []string{"Down", "Down", "Enter"}) {
		t.Errorf("keys = %v, want the cursor moved to No", keys)
	}
	got, _ := store.GetTask(context.Background(), task.ID)
	if got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("task status = %s, want awaiting-input", got.Status)
	}
}

// Words instead of an answer deny the permission, then go to the agent
// as the next turn.
func TestDispatch_PromptRepliedInWords(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	task := stopAtPrompt(t, store, r, ws)

	exec.set(func() {
		exec.capture = "Interrupted"
		exec.onSendKeys = func() { exec.capture, exec.fileExists = "used make clean", true }
	})
	reply, err := r.Dispatch(context.Background(), "conv-1", "use make clean instead")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if reply != "relayed: used make clean" {
		t.Errorf("reply = %q", reply)
	}
	s := exec.sessionFor(task.TmuxSession)
	if !slices.Equal(s.namedKeys, []string{"Down", "Down", "Enter"}) || !slices.Contains(s.keys, "use make clean instead") {
		t.Errorf("named keys %v, typed %q; want No chosen, then the words sent", s.namedKeys, s.keys)
	}
}

// An option number out of range isn't sent anywhere: the user is told,
// and the prompt still waits.
func TestDispatch_PromptBadOption(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	task := stopAtPrompt(t, store, r, ws)
	reply, err := r.Dispatch(context.Background(), "conv-1", "7")
	if err != nil || !strings.Contains(reply, "no option 7") {
		t.Errorf("reply %q, err %v; want it to say there's no option 7", reply, err)
	}
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; len(keys) != 0 {
		t.Errorf("keys = %v, want none", keys)
	}
	if got, _ := store.GetTask(context.Background(), task.ID); got.Status != registry.TaskStatusNeedsAttention {
		t.Errorf("task status = %s, want still needs-attention", got.Status)
	}
}

// A sign-in screen fails fast with login_required, the pane kept for a
// human to finish the login in.
func TestDispatch_LoginRequired(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	exec.capture = "LOGIN"
	start := time.Now()
	_, err := r.Dispatch(context.Background(), "conv-1", "hello")
	if err == nil {
		t.Fatal("Dispatch: nil error for an agent that isn't signed in")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s, want it fast", time.Since(start))
	}
	task := onlyTask(t, store, ws.ID)
	for _, want := range []string{"isn't signed in", "tmux -L loomux attach -t " + task.TmuxSession} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	var attention *orchestrator.NeedsAttentionError
	if errors.As(err, &attention) {
		t.Error("the login error shouldn't wrap the detector's error")
	}
	assertFailed(t, task, registry.ErrorClassLoginRequired, "isn't signed in")
	if got := router.ClassifyError(err); got != registry.ErrorClassLoginRequired {
		t.Errorf("ClassifyError = %q, want login_required for the dispatch job", got)
	}
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the pane was torn down; keep it for the login")
	}
}

// LOOM-109: an agent at its usage limit fails the turn with
// agent_rate_limited and says when the limit resets, instead of the
// error being relayed as an answer. The marker fires at once here, so
// the final-screen check is what catches it if the watcher hasn't yet.
func TestDispatch_UsageLimit(t *testing.T) {
	store, exec, r, ws := attentionHarness(t)
	exec.set(func() {
		exec.capture = "LIMIT"
		exec.fileExists = true
	})
	_, err := r.Dispatch(context.Background(), "conv-1", "hello")
	if err == nil {
		t.Fatal("Dispatch: nil error for an agent at its usage limit")
	}
	for _, want := range []string{"hit its usage limit", "resets 5pm (Europe/Istanbul)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassAgentRateLimited {
		t.Errorf("ClassifyError = %q, want agent_rate_limited for the dispatch job", got)
	}
	task := onlyTask(t, store, ws.ID)
	assertFailed(t, task, registry.ErrorClassAgentRateLimited, "resets 5pm (Europe/Istanbul)")
}
