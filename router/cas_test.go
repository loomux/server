package router_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// LOOM-262: a status change made while a turn waits (later, a human
// takeover; written straight to the store here) survives the wait's end
// and whatever the turn does after it: the task is left as it is, not
// failed, and nothing is typed into its pane.

// hookedDetector runs onWait as the first wait starts, then waits as
// inner does.
type hookedDetector struct {
	inner  orchestrator.CompletionDetector
	once   sync.Once
	onWait func(task *registry.Task)
}

func (d *hookedDetector) Wait(ctx context.Context, task *registry.Task) error {
	if d.onWait != nil {
		d.once.Do(func() { d.onWait(task) })
	}
	return d.inner.Wait(ctx, task)
}

type casHarness struct {
	store    registry.Store
	exec     *fakeExecutor
	r        *router.Router
	ws       *registry.Workspace
	detector *hookedDetector
	relayed  atomic.Int32
	// relay, if set, is the relay model's answer; else "ok", not done.
	relay func(ctx context.Context, captured string) (router.RelayResult, error)
}

func newCASHarness(t *testing.T, agent router.AgentType) *casHarness {
	t.Helper()
	h := &casHarness{store: newTestStore(t), exec: newFakeExecutor()}
	agentTypes := router.AgentTypeRegistry{
		"":            router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": agent,
	}
	markerDir := t.TempDir()
	h.detector = &hookedDetector{inner: completion.NewDetector(h.store, h.exec.factory(), agentTypes.CompletionConfig(), markerDir,
		completion.WithProgressPollInterval(10*time.Millisecond), completion.WithPollInterval(20*time.Millisecond))}
	orch := orchestrator.New(h.store, h.exec.factory(), h.detector)
	h.ws = createFixtureWorkspace(t, h.store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "claude-code"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			h.relayed.Add(1)
			if h.relay != nil {
				return h.relay(ctx, captured)
			}
			return router.RelayResult{Reply: "ok"}, nil
		},
	}
	h.r = router.New(h.store, orch, h.exec.factory(), credentials.NewResolver(h.store), agentTypes, model, markerDir)
	return h
}

// setStatus is the change made meanwhile, straight to the store.
func (h *casHarness) setStatus(t *testing.T, taskID string, status registry.TaskStatus) {
	t.Helper()
	task, err := h.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Errorf("GetTask: %v", err)
		return
	}
	task.Status = status
	if err := h.store.UpdateTask(context.Background(), task); err != nil {
		t.Errorf("UpdateTask: %v", err)
	}
}

// takeOverDuringTheWait makes the first wait's "meanwhile" a takeover.
func (h *casHarness) takeOverDuringTheWait(t *testing.T) {
	h.detector.onWait = func(task *registry.Task) { h.setStatus(t, task.ID, registry.TaskStatusHumanTakeover) }
}

// requireLeftTakenOver: the task is still human_takeover with nothing of
// a failure, completion or prompt written over it, and no key reached
// its pane by name (an interrupt).
func (h *casHarness) requireLeftTakenOver(t *testing.T) *registry.Task {
	t.Helper()
	task := onlyTask(t, h.store, h.ws.ID)
	if task.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("status = %s, want human_takeover kept: the turn overwrote it (reason %q, class %q)",
			task.Status, task.FailureReason, task.ErrorClass)
	}
	if task.FailureReason != "" || task.ErrorClass != "" || task.CompletedAt != nil || task.Attention != nil {
		t.Fatalf("task carries a write the turn lost: %+v", task)
	}
	var named []string
	h.exec.set(func() {
		if s := h.exec.sessions[task.TmuxSession]; s != nil {
			named = slices.Clone(s.namedKeys)
		}
	})
	if len(named) != 0 {
		t.Fatalf("keys %v were sent into a pane someone else now has", named)
	}
	return task
}

// WaitForCompletion: the turn's end doesn't write awaiting_input over the
// takeover, and the turn stops there — nothing is relayed for it.
func TestDispatch_TakeoverDuringTheWaitSurvivesItsEnd(t *testing.T) {
	h := newCASHarness(t, router.AgentType{
		AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond},
		LaunchTemplate: "claude",
	})
	h.takeOverDuringTheWait(t)

	_, err := h.r.Dispatch(context.Background(), "conv-1", "hello")
	if !errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("Dispatch err = %v, want ErrStatusChanged", err)
	}
	h.requireLeftTakenOver(t)
	if n := h.relayed.Load(); n != 0 {
		t.Fatalf("the turn was relayed %d times after its task was taken over", n)
	}
}

// failTask after the wait: a turn that times out while taken over isn't
// failed, and its agent isn't interrupted.
func TestDispatch_TakeoverDuringATimedOutWaitIsNotFailed(t *testing.T) {
	h := newCASHarness(t, router.AgentType{
		AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, NoProgressTimeout: -1},
		LaunchTemplate: "claude",
		InterruptKeys:  []string{"Escape"},
	})
	h.takeOverDuringTheWait(t)

	_, err := h.r.Dispatch(context.Background(), "conv-1", "do something long")
	if !errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("Dispatch err = %v, want ErrStatusChanged", err)
	}
	h.requireLeftTakenOver(t)
}

// promptAgent is a marker-tier agent whose pane shows screen, read as a
// prompt by kind.
func promptAgent(screens map[string]*registry.Attention) router.AgentType {
	return router.AgentType{
		AgentConfig: completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: time.Minute, NoProgressTimeout: time.Minute,
			DetectPrompt: func(screen string) *registry.Attention {
				if a, ok := screens[screen]; ok {
					cp := *a
					return &cp
				}
				return nil
			}},
		LaunchTemplate: "claude",
	}
}

// The attention path: a sign-in screen fails the task (login_required) —
// but not one taken over while the turn waited.
func TestDispatch_TakeoverDuringTheWaitSurvivesALoginPrompt(t *testing.T) {
	h := newCASHarness(t, promptAgent(map[string]*registry.Attention{
		"LOGIN": {Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: "Select login method:"},
	}))
	h.exec.capture = "LOGIN"
	h.takeOverDuringTheWait(t)

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "hello"); err == nil {
		t.Fatal("Dispatch: nil error for a turn whose task was taken over")
	}
	h.requireLeftTakenOver(t)
}

// The attention path: a permission prompt makes the task needs-attention
// — but not one taken over while the turn waited.
func TestDispatch_TakeoverDuringTheWaitSurvivesAPermissionPrompt(t *testing.T) {
	h := newCASHarness(t, promptAgent(map[string]*registry.Attention{
		permissionScreen: {Kind: registry.AttentionPermission, Title: "Bash command", Question: "Do you want to proceed?",
			Options: []registry.AttentionOption{{Label: "Yes"}, {Label: "No"}}},
	}))
	h.exec.capture = permissionScreen
	h.takeOverDuringTheWait(t)

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "clean the build"); err == nil {
		t.Fatal("Dispatch: nil error for a turn whose task was taken over")
	}
	h.requireLeftTakenOver(t)
}

// Complete after the relay (an external call): a task taken over while
// its turn was being relayed isn't completed. The relayed reply is the
// agent's real answer and is still returned.
func TestDispatch_TakeoverDuringTheRelaySurvivesComplete(t *testing.T) {
	h := newCASHarness(t, router.AgentType{
		AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond},
		LaunchTemplate: "claude",
	})
	h.relay = func(ctx context.Context, captured string) (router.RelayResult, error) {
		h.setStatus(t, onlyTask(t, h.store, h.ws.ID).ID, registry.TaskStatusHumanTakeover)
		return router.RelayResult{Reply: "All finished.", Done: true}, nil
	}

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "wrap it up")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "All finished." {
		t.Fatalf("reply = %q, want the relayed answer", reply)
	}
	h.requireLeftTakenOver(t)
}
