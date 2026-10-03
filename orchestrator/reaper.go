package orchestrator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/registry"
)

// Reaper periodically calls Orchestrator.Reap on tasks that have gone
// idle past a configured threshold (design spec's continuation model,
// LOOM-13/16): a task left open for a follow-up (RelayResult.Done ==
// false, per router) shouldn't hold its tmux session — and whatever
// credential material is only live in that process's environment — open
// forever if no follow-up ever comes.
//
// "Idle" is measured from registry.Task.UpdatedAt, not from any
// conversation-level "last human message" concept — UpdatedAt only
// advances when the orchestrator itself changes the task's state
// (SendMessage, a completion signal via WaitForCompletion, Takeover,
// Release), which is exactly "last agent activity" (a completion signal
// firing is literally the agent's last tool call/output going idle).
//
// HumanTakeover tasks are deliberately excluded from every sweep: once a
// human takes over, further interaction happens by attaching directly to
// the pane (send-keys typed by hand), which never goes through the
// orchestrator and so never advances UpdatedAt — treating a stale
// UpdatedAt as "idle" during an active takeover would risk killing a
// session a human is using right now. There's no reliable activity
// signal for a takeover session available here to reap it safely by.
//
// Running tasks are excluded too, and for a similarly important reason
// (caught by a real end-to-end test, not by inspection): Running means a
// turn is actively in flight — SendMessage was just called and Router is
// blocked in WaitForCompletion, possibly for a long time on a legitimately
// long-running turn. UpdatedAt only advances again once that turn's
// completion signal fires, so a Running task's UpdatedAt ages exactly
// like a genuinely-idle one even though nothing is idle at all. Only
// AwaitingInput — set precisely when a turn concludes and Router leaves
// it open for a follow-up that hasn't arrived yet — is a state where
// "idle since UpdatedAt" actually means "idle."
//
// The sweep also resolves workspaces stuck provisioning (LOOM-60): the
// router bounds its own provisioning and fails the workspace when that
// bound passes, but only while the process that started it is alive. A
// row still provisioning well past that bound — left by a restart
// mid-provisioning, or written before provisioning was bounded — has no
// provisioner left to finish or fail it, so it is marked failed with a
// reason (the row is kept for inspection, like any failed workspace).
type Reaper struct {
	orch      *Orchestrator
	threshold time.Duration
	logf      func(format string, args ...any)
	metrics   *metrics.Metrics
	// staleProvisioningAfter is how long a workspace may stay
	// provisioning before the sweep fails it; zero disables that check.
	staleProvisioningAfter time.Duration
}

// ReaperOption configures a Reaper constructed via NewReaper.
type ReaperOption func(*Reaper)

// WithReaperLogger overrides where Reap events are logged. Default:
// log.Printf — this repo has no logging convention of its own yet (same
// gap noted in api's login-throttle work, LOOM-15).
func WithReaperLogger(logf func(format string, args ...any)) ReaperOption {
	return func(r *Reaper) { r.logf = logf }
}

// WithMetrics sets the Prometheus metrics bundle the reaper should record
// into (LOOM-103). A nil value is accepted and ignored.
func WithReaperMetrics(m *metrics.Metrics) ReaperOption {
	return func(r *Reaper) { r.metrics = m }
}

// WithStaleProvisioningAfter makes each sweep fail workspaces still
// provisioning longer than d after they were created (LOOM-60). d must
// exceed the router's own provisioning bound, so a live provisioning is
// never touched. Zero (the default) disables the check.
func WithStaleProvisioningAfter(d time.Duration) ReaperOption {
	return func(r *Reaper) { r.staleProvisioningAfter = d }
}

// NewReaper constructs a Reaper. threshold is how long a reapable task
// can go without a state change before Sweep tears its session down.
func NewReaper(orch *Orchestrator, threshold time.Duration, opts ...ReaperOption) *Reaper {
	r := &Reaper{orch: orch, threshold: threshold, logf: log.Printf}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run blocks, calling Sweep every interval until ctx is done. Stuck
// provisioning is checked once immediately as well, so rows a restart
// left behind are resolved at startup rather than an interval later.
func (r *Reaper) Run(ctx context.Context, interval time.Duration) {
	if workspaces, err := r.orch.store.ListWorkspaces(ctx); err == nil {
		r.failStaleProvisioning(ctx, workspaces, time.Now().UTC())
	} else {
		r.logf("orchestrator: reaper: startup: list workspaces: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep runs one pass immediately, across every workspace: every
// reapable task idle past threshold gets torn down via Orchestrator.Reap.
// Exported so callers (and tests) can trigger a deterministic pass
// instead of waiting on Run's ticker.
func (r *Reaper) Sweep(ctx context.Context) {
	workspaces, err := r.orch.store.ListWorkspaces(ctx)
	if err != nil {
		r.logf("orchestrator: reaper: sweep: list workspaces: %v", err)
		return
	}

	now := time.Now().UTC()
	r.failStaleProvisioning(ctx, workspaces, now)
	for _, ws := range workspaces {
		tasks, err := r.orch.store.ListTasksByWorkspace(ctx, ws.ID)
		if err != nil {
			r.logf("orchestrator: reaper: sweep: list tasks for workspace %s: %v", ws.ID, err)
			continue
		}
		for _, task := range tasks {
			if !isReapable(task) {
				continue
			}
			idleFor := now.Sub(task.UpdatedAt)
			if idleFor < r.threshold {
				continue
			}
			if err := r.orch.Reap(ctx, task.ID); err != nil {
				r.logf("orchestrator: reaper: reap task %s: %v", task.ID, err)
				continue
			}
			r.logf("orchestrator: reaper: reaped task %s (idle %s)", task.ID, idleFor.Round(time.Second))
			if r.metrics != nil {
				r.metrics.RecordReaperTask()
			}
		}
	}
}

// failStaleProvisioning marks failed every workspace in workspaces that
// has been provisioning longer than staleProvisioningAfter (LOOM-60). The
// status is re-read just before the write, so a provisioning that
// finished since the list was taken is left as it ended.
func (r *Reaper) failStaleProvisioning(ctx context.Context, workspaces []*registry.Workspace, now time.Time) {
	if r.staleProvisioningAfter <= 0 {
		return
	}
	for _, ws := range workspaces {
		if ws.Status != registry.WorkspaceStatusProvisioning || now.Sub(ws.CreatedAt) < r.staleProvisioningAfter {
			continue
		}
		cur, err := r.orch.store.GetWorkspace(ctx, ws.ID)
		if err != nil {
			r.logf("orchestrator: reaper: stale provisioning: get workspace %s: %v", ws.ID, err)
			continue
		}
		if cur.Status != registry.WorkspaceStatusProvisioning {
			continue
		}
		age := now.Sub(cur.CreatedAt).Round(time.Second)
		cur.Status = registry.WorkspaceStatusFailed
		cur.StatusReason = fmt.Sprintf("provisioning did not finish: still provisioning %s after the workspace was created, "+
			"with nothing left running it (Loomux restarted mid-provisioning, or the row predates bounded provisioning)", age)
		if err := r.orch.store.UpdateWorkspace(ctx, cur); err != nil {
			r.logf("orchestrator: reaper: stale provisioning: mark workspace %s failed: %v", ws.ID, err)
			continue
		}
		r.logf("orchestrator: reaper: marked workspace %s failed (provisioning for %s)", ws.ID, age)
	}
}

// isReapable reports whether t is a candidate for idle-reaping at all —
// AwaitingInput only. See the Reaper doc comment for why Running and
// HumanTakeover are excluded; Completed/Failed are already terminal, no
// live session to speak of.
func isReapable(t *registry.Task) bool {
	return t.Status == registry.TaskStatusAwaitingInput
}
