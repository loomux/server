package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Launch creates a new task (minting a fresh UUID for it) and starts its
// tmux session. See LaunchWithID for the full behavior — Launch is a
// thin wrapper for the common case where the caller has no need to know
// the task's ID before command is already built.
func (o *Orchestrator) Launch(ctx context.Context, workspaceID, conversationID string, kind registry.TaskKind, agentType, command string) (*registry.Task, error) {
	return o.LaunchWithID(ctx, workspaceID, conversationID, kind, agentType, uuid.NewString(), command)
}

// LaunchWithID is Launch, but with the task's ID supplied by the caller
// instead of minted here. command is assumed to already be a complete,
// resolved launch command (credential injection and agent-type→command-
// template resolution are separate concerns, not this package's job).
// agentType is only stored for TaskKindAgent.
//
// This seam exists for a caller — router.launchAgent — that needs to
// know a task's ID before command itself is built (LOOM-32: a
// TierMarker agent-type's launch command must embed a per-task
// completion-marker path, which is only meaningful once the task ID
// exists; Launch alone can't support that, since it doesn't mint the ID
// until after the caller has already handed it a finished command).
// taskID must be non-empty and caller-unique (a UUID, same as Launch's
// own default) — LaunchWithID doesn't validate uniqueness itself; a
// collision surfaces as whatever error the store's CreateTask returns
// for a duplicate primary key.
//
// LaunchWithID always returns a non-nil *registry.Task once its row has
// been created, even if starting the session subsequently fails (in
// which case the returned task is already Failed and the error
// describes why) — the row stays visible/auditable rather than
// vanishing on failure.
func (o *Orchestrator) LaunchWithID(ctx context.Context, workspaceID, conversationID string, kind registry.TaskKind, agentType, taskID, command string) (*registry.Task, error) {
	ws, err := o.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: launch: %w", err)
	}
	target, err := o.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: launch: %w", err)
	}
	exec, err := o.newExecutor(target)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: launch: %w", err)
	}

	now := time.Now().UTC()
	task := &registry.Task{
		ID:             taskID,
		WorkspaceID:    workspaceID,
		Kind:           kind,
		TmuxSession:    "loomux-" + uuid.NewString(),
		Status:         registry.TaskStatusRunning,
		ConversationID: conversationID,
		StartedAt:      &now,
	}
	switch kind {
	case registry.TaskKindAgent:
		task.AgentType = agentType
	case registry.TaskKindCommand:
		// Only a command task records what it ran (LOOM-71): an agent's
		// launch command carries injected credentials and is never stored.
		task.Command = command
	}

	if err := o.store.CreateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("orchestrator: launch: %w", err)
	}
	o.recordTaskTransition("", string(task.Status), kind)

	// Every pane with a command reports its own exit status (LOOM-136):
	// a command task's recipe, and an agent CLI that exits (127 when it
	// isn't installed) without waiting on tmux to reap it.
	paneCommand := command
	if command != "" {
		paneCommand = targets.WrapExitStatus(command)
	}
	if err := exec.NewSession(ctx, task.TmuxSession, ws.Path, paneCommand); err != nil {
		// best-effort; original err is what matters to the caller
		_ = o.failTask(ctx, task, task.Status, failureFor(registry.ErrorClassLaunchFailed, "start session", err))
		return task, fmt.Errorf("orchestrator: launch: %w", err)
	}

	// A provisioning workspace stays provisioning while its setup runs:
	// the router owns that status (→ idle on success, → failed
	// otherwise, LOOM-77) — "active" would claim it's ready and busy.
	if ws.Status != registry.WorkspaceStatusProvisioning {
		ws.Status = registry.WorkspaceStatusActive
	}
	ws.LastUsedAt = &now
	if err := o.store.UpdateWorkspace(ctx, ws); err != nil {
		return task, fmt.Errorf("orchestrator: launch: %w", err)
	}

	return task, nil
}

// SendMessage sends message as the next turn's input into a running
// task. Refuses if the task is under human takeover or is no longer
// active. The paste is an external call: the status write after it is a
// compare-and-set against the status read before it (LOOM-262), and a
// change made meanwhile is ErrStatusChanged.
func (o *Orchestrator) SendMessage(ctx context.Context, taskID, message string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: send message: %w", err)
	}

	switch task.Status {
	case registry.TaskStatusHumanTakeover:
		return ErrHumanTakeover
	case registry.TaskStatusCompleted, registry.TaskStatusFailed:
		return ErrTaskInactive
	}

	exec, err := o.executorFor(ctx, task)
	if err != nil {
		return fmt.Errorf("orchestrator: send message: %w", err)
	}

	// Pasted, not typed (LOOM-111): a newline in the message mustn't press
	// Enter in the agent's input before the rest has arrived.
	if err := exec.PasteText(ctx, task.TmuxSession, message, true); err != nil {
		if errors.Is(err, targets.ErrTextTooLarge) {
			// Nothing reached the pane: the task is as it was.
			return fmt.Errorf("orchestrator: send message: %w", err)
		}
		// tmux refuses a paste into a pane whose process has exited (an
		// agent CLI that isn't installed dies at once): that exit is the
		// news, as when the wait finds it, not the refused paste.
		if exit, perr := exec.PaneExited(ctx, task.TmuxSession); perr == nil && exit != nil {
			return fmt.Errorf("orchestrator: send message: %w", &ProcessExitedError{Status: exit.Status, Output: exit.Output})
		}
		_ = o.failTask(ctx, task, task.Status, failureFor(registry.ErrorClassSendFailed, "send message", err))
		return fmt.Errorf("orchestrator: send message: %w", err)
	}

	from := task.Status
	task.Status = registry.TaskStatusRunning
	if err := o.transition(ctx, "send message", task, from); err != nil {
		return fmt.Errorf("orchestrator: send message: %w", err)
	}
	return nil
}

// WaitForCompletion blocks until the CompletionDetector reports the
// task's current turn is done, or ctx is done. On success the task
// transitions to AwaitingInput ("turn done, ready for the next
// message"). A context/detector error is returned as-is without any
// status transition — the wait giving up doesn't mean the task itself
// failed.
//
// The transition is a compare-and-set against the status read before the
// wait (LOOM-262): if something changed it while the turn ran (a cancel;
// later, a human takeover), that change is kept, the task is left as it
// is, and the result is ErrStatusChanged — the turn is no longer the
// caller's to finish. A turn is only ever waited on from running (its
// caller sent it, resumed it or launched it): a task read in any other
// status was changed after that, so it isn't waited on at all.
func (o *Orchestrator) WaitForCompletion(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: wait for completion: %w", err)
	}
	if task.Status != registry.TaskStatusRunning {
		return fmt.Errorf("orchestrator: wait for completion: %w",
			o.statusChanged(ctx, "wait for completion", task.ID, registry.TaskStatusRunning, registry.TaskStatusAwaitingInput))
	}

	if err := o.detector.Wait(ctx, task); err != nil {
		return err
	}

	task.Status = registry.TaskStatusAwaitingInput
	if err := o.transition(ctx, "wait for completion", task, registry.TaskStatusRunning); err != nil {
		return fmt.Errorf("orchestrator: wait for completion: %w", err)
	}
	return nil
}

// Complete marks the task finished. Its session is left running for a
// grace period (LOOM-91), so a person can still attach and see what the
// agent did; a FinishedPaneSweeper tears it down after. summary is applied
// to the workspace's rolling summary verbatim — Complete never generates
// or condenses it itself (that's the router's job).
//
// from is the status the caller read before deciding the task is done —
// after the turn's wait and its relay, an external call — and the write
// is a compare-and-set against it (LOOM-262): a task whose status changed
// meanwhile is left as it is, its workspace and summary untouched, and
// the result is ErrStatusChanged.
func (o *Orchestrator) Complete(ctx context.Context, taskID string, from registry.TaskStatus, summary string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: complete: %w", err)
	}

	now := time.Now().UTC()
	task.Status = registry.TaskStatusCompleted
	task.CompletedAt = &now
	if err := o.transition(ctx, "complete", task, from); err != nil {
		return fmt.Errorf("orchestrator: complete: %w", err)
	}

	if err := o.store.SetWorkspaceRollingSummary(ctx, task.WorkspaceID, summary); err != nil {
		return fmt.Errorf("orchestrator: complete: %w", err)
	}

	ws, err := o.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return fmt.Errorf("orchestrator: complete: %w", err)
	}
	ws.Status = registry.WorkspaceStatusIdle
	if err := o.store.UpdateWorkspace(ctx, ws); err != nil {
		return fmt.Errorf("orchestrator: complete: %w", err)
	}
	return nil
}

// FinishCommand records a TaskKindCommand task's exit code once its
// process has exited (completion.TierExit), tears its pane down and marks
// it completed — a non-zero exit is still a command that ran to the end,
// so the code, not the status, says how it went. Unlike Complete it
// leaves the workspace's rolling summary alone: a command's output goes
// back to chat, it isn't a summary of the workspace's work.
//
// from is the status the caller read before the command's wait and its
// exit-status read; a task no longer in it is left as it is, its pane not
// killed (someone else may have it), and the result is ErrStatusChanged
// (LOOM-262). The completion itself is a compare-and-set against from.
func (o *Orchestrator) FinishCommand(ctx context.Context, taskID string, from registry.TaskStatus, exitCode int) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}
	if task.Kind != registry.TaskKindCommand {
		return fmt.Errorf("orchestrator: finish command: task %q is a %s task, not a command", taskID, task.Kind)
	}
	if task.Status != from {
		return fmt.Errorf("orchestrator: finish command: %w", o.statusChanged(ctx, "finish command", task.ID, from, registry.TaskStatusCompleted))
	}
	exec, err := o.executorFor(ctx, task)
	if err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}
	if err := exec.KillSession(ctx, task.TmuxSession); err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}

	now := time.Now().UTC()
	task.Status = registry.TaskStatusCompleted
	task.CompletedAt = &now
	task.ExitCode = &exitCode
	if err := o.transition(ctx, "finish command", task, from); err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}

	ws, err := o.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}
	// A provisioning recipe's workspace stays provisioning: the router
	// sets it idle with the path the recipe printed, or failed (see
	// Launch) — idle here would be a workspace without its path.
	if ws.Status != registry.WorkspaceStatusActive {
		return nil
	}
	ws.Status = registry.WorkspaceStatusIdle
	if err := o.store.UpdateWorkspace(ctx, ws); err != nil {
		return fmt.Errorf("orchestrator: finish command: %w", err)
	}
	return nil
}

// Fail marks a task failed, recording why (LOOM-77), without tearing
// down its session — the spec leaves a failed pane alive for inspection
// via attach, since teardown is for successful/explicit completion, not
// silent cleanup of something that went wrong.
//
// from is the status the caller read before deciding the task failed —
// often before a wait or an external call — and the write is a
// compare-and-set against it (LOOM-262): a task whose status changed
// meanwhile is left as it is, not failed, and the result is
// ErrStatusChanged.
func (o *Orchestrator) Fail(ctx context.Context, taskID string, from registry.TaskStatus, failure registry.TaskFailure) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: fail: %w", err)
	}
	if err := o.failTask(ctx, task, from, failure); err != nil {
		return fmt.Errorf("orchestrator: fail: %w", err)
	}
	return nil
}

// failureFor builds the TaskFailure for an operation that failed with
// err, classing an unreachable target as such whatever the operation.
func failureFor(class registry.ErrorClass, op string, err error) registry.TaskFailure {
	if errors.Is(err, targets.ErrUnreachable) {
		class = registry.ErrorClassTargetUnreachable
	}
	return registry.TaskFailure{Class: class, Reason: op + ": " + err.Error()}
}

// Reap tears down an idle task's session (design spec's continuation
// model, LOOM-16): the tmux pane is killed if it still exists — which
// also destroys any credential material a launch injected as env vars,
// the only place it lives beyond the stateless per-call resolution in
// credentials.Resolver.Resolve, so no separate vault "release" call
// exists to make. Idempotent: a session already gone for some other
// reason (crash, manual kill) is not an error.
//
// Unlike Fail, Status is deliberately left unchanged and the workspace
// is not reverted to Idle — a reaped task is still logically open
// (Running/AwaitingInput) as far as the conversation goes; only the
// session is gone. ReapedAt is set purely for visibility. The next
// message routed to this task discovers the session missing (a live
// HasSession check) and the router falls back to a fresh task,
// transitioning this one to Failed at that point — see
// router.dispatchToAgent.
func (o *Orchestrator) Reap(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reap: %w", err)
	}

	exec, err := o.executorFor(ctx, task)
	if err != nil {
		return fmt.Errorf("orchestrator: reap: %w", err)
	}

	live, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		return fmt.Errorf("orchestrator: reap: %w", err)
	}
	if live {
		if err := exec.KillSession(ctx, task.TmuxSession); err != nil {
			return fmt.Errorf("orchestrator: reap: kill session: %w", err)
		}
	}

	// Only reaped_at: the row read above may be stale by now (LOOM-120).
	if err := o.store.SetTaskReapedAt(ctx, task.ID, time.Now().UTC()); err != nil {
		return fmt.Errorf("orchestrator: reap: %w", err)
	}
	return nil
}

// executorFor resolves the TargetExecutor for the target a task's
// workspace runs on.
func (o *Orchestrator) executorFor(ctx context.Context, task *registry.Task) (targets.TargetExecutor, error) {
	ws, err := o.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	target, err := o.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, err
	}
	return o.newExecutor(target)
}

// failTask transitions task to Failed, recording failure, and an active
// workspace back to Idle — a workspace in any other status (provisioning,
// failed, archived) keeps it: its status is about the workspace, not this
// one task. Shared by the public Fail and by Launch/SendMessage's internal
// auto-fail-on-executor-error path. The write is a compare-and-set
// against from (LOOM-262); losing it leaves the task and its workspace as
// they are and returns ErrStatusChanged.
func (o *Orchestrator) failTask(ctx context.Context, task *registry.Task, from registry.TaskStatus, failure registry.TaskFailure) error {
	now := time.Now().UTC()
	task.Status = registry.TaskStatusFailed
	task.CompletedAt = &now
	task.FailureReason = failure.Reason
	task.ErrorClass = failure.Class
	task.OutputTail = failure.OutputTail
	task.Attention = nil
	if err := o.transition(ctx, "fail", task, from); err != nil {
		return err
	}

	ws, err := o.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return err
	}
	if ws.Status != registry.WorkspaceStatusActive {
		return nil
	}
	ws.Status = registry.WorkspaceStatusIdle
	return o.store.UpdateWorkspace(ctx, ws)
}

// NeedAttention records that task's agent is stopped at a prompt only a
// human can answer (LOOM-97): status needs-attention, with the prompt.
// Its pane is left as it is, showing the prompt.
//
// from is the status the caller read before the wait that found the
// prompt — running, or needs-attention for a prompt replaced by another —
// and the write is a compare-and-set against it (LOOM-262): a task whose
// status changed meanwhile is left as it is and the result is
// ErrStatusChanged.
func (o *Orchestrator) NeedAttention(ctx context.Context, taskID string, from registry.TaskStatus, attention *registry.Attention) error {
	if from != registry.TaskStatusRunning && from != registry.TaskStatusNeedsAttention {
		return fmt.Errorf("orchestrator: need attention: task %q is %s: %w", taskID, from, ErrTaskInactive)
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: need attention: %w", err)
	}
	task.Status, task.Attention = registry.TaskStatusNeedsAttention, attention
	if err := o.transition(ctx, "need attention", task, from); err != nil {
		return fmt.Errorf("orchestrator: need attention: %w", err)
	}
	return nil
}

// Resume returns a needs-attention task to running once its prompt has
// been answered (LOOM-97), clearing the prompt. The answer's keys were
// typed after the caller read the task needs-attention, so the write is a
// compare-and-set from needs-attention (LOOM-262): a task no longer
// waiting at its prompt is left as it is and the result is
// ErrStatusChanged.
func (o *Orchestrator) Resume(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: resume: %w", err)
	}
	task.Status, task.Attention = registry.TaskStatusRunning, nil
	if err := o.transition(ctx, "resume", task, registry.TaskStatusNeedsAttention); err != nil {
		return fmt.Errorf("orchestrator: resume: %w", err)
	}
	return nil
}

// transition writes task's status change as a compare-and-set from
// from (LOOM-262), recording the transition once it is written. Losing it
// writes nothing: it is logged and returned as ErrStatusChanged.
func (o *Orchestrator) transition(ctx context.Context, op string, task *registry.Task, from registry.TaskStatus) error {
	applied, err := o.store.TransitionTask(ctx, task, from)
	if err != nil {
		return err
	}
	if !applied {
		return o.statusChanged(ctx, op, task.ID, from, task.Status)
	}
	if from != task.Status {
		o.recordTaskTransition(string(from), string(task.Status), task.Kind)
	}
	return nil
}

// statusChanged logs and returns a lost compare-and-set: op wanted to
// move taskID from from to to, but its status had changed meanwhile.
func (o *Orchestrator) statusChanged(ctx context.Context, op, taskID string, from, to registry.TaskStatus) error {
	now := "unknown"
	if cur, err := o.store.GetTask(context.WithoutCancel(ctx), taskID); err == nil {
		now = string(cur.Status)
	}
	o.logger.Warn("task status changed meanwhile; left as it is", "op", op, "task_id", taskID,
		"read_status", string(from), "status", now, "not_written", string(to))
	return fmt.Errorf("%w: task %q is %s, no longer %s", ErrStatusChanged, taskID, now, from)
}
