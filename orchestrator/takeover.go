package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// A hold's lease length (LOOM-264): what a takeover gets when it doesn't
// ask, and the bounds on what it may ask for. A client heartbeats every
// third of the lease, so the default tolerates two lost heartbeats.
const (
	DefaultHoldTTL = 45 * time.Second
	MinHoldTTL     = 15 * time.Second
	MaxHoldTTL     = 300 * time.Second
)

// takeoverAttempts bounds how often Takeover starts over when the task
// or its hold changed between its read and its write.
const takeoverAttempts = 4

// ErrUnsupportedHoldMode is returned by Takeover for a mode this server
// knows but doesn't serve yet: watch, until watch holds exist. Nothing
// was taken, so a client never mistakes the answer for a drive hold.
var ErrUnsupportedHoldMode = errors.New("orchestrator: hold mode not supported")

// ErrHoldNotOwned is returned by Heartbeat when the hold belongs to
// another session: only its holder keeps a lease alive.
var ErrHoldNotOwned = errors.New("orchestrator: hold belongs to another session")

// InvalidTakeoverError is returned by Takeover for a request that can't
// be served as asked; Reason is worded for the client.
type InvalidTakeoverError struct{ Reason string }

func (e *InvalidTakeoverError) Error() string { return "orchestrator: takeover: " + e.Reason }

// TaskHeldError is returned by Takeover when another session holds the
// task and the request didn't force it. Hold is that session's hold.
type TaskHeldError struct{ Hold *registry.TaskHold }

func (e *TaskHeldError) Error() string {
	return fmt.Sprintf("orchestrator: task %q is held by another session", e.Hold.TaskID)
}

// HoldEndedError is returned by Heartbeat for a hold that has ended.
// Hold says when and why.
type HoldEndedError struct{ Hold *registry.TaskHold }

func (e *HoldEndedError) Error() string {
	return fmt.Sprintf("orchestrator: hold %q has ended (%s)", e.Hold.ID, e.Hold.EndedReason)
}

// TakeoverRequest asks for a hold on a task (LOOM-264,
// docs/design/tui-client-design.md "Server awareness: holds").
type TakeoverRequest struct {
	TaskID string
	// SessionID is the API session asking; a hold belongs to it.
	SessionID string
	// ClientKind and ClientLabel describe the holder to other clients.
	ClientKind  string
	ClientLabel string
	// Mode is drive, or empty for drive.
	Mode registry.HoldMode
	// TTL is the lease's length, MinHoldTTL to MaxHoldTTL; zero is
	// DefaultHoldTTL.
	TTL time.Duration
	// Force takes the task from another session that holds it.
	Force bool
}

// TakeoverResult is what Takeover did.
type TakeoverResult struct {
	Hold *registry.TaskHold
	// Task is the task as it stands with the hold.
	Task *registry.Task
	// Renewed: the session already held the task, and its hold was
	// renewed and returned rather than a new one made.
	Renewed bool
	// Replaced is the other session's hold a forced takeover ended.
	Replaced *registry.TaskHold
}

// Takeover gives req's session the drive hold on a task: the task
// becomes human-takeover, which pauses automated dispatch into its pane
// until the hold is released, and the hold records who has it.
//
// The task's active drive hold is looked at before its status:
//
//   - Held by this session: the hold is renewed and returned. This is
//     idempotent, and how a restarted client gets its own hold back.
//   - Held by another session: *TaskHeldError with that hold, unless
//     req.Force, which ends it as taken_over and gives this session a
//     new one. The task stays as it is. If the task ended while held
//     (nothing ends its hold yet), it is ErrTaskInactive either way.
//   - Not held: a running, awaiting-input or needs-attention task moves
//     to human-takeover; one already in human-takeover with no hold (only
//     possible from before holds existed) is taken where it stands. A
//     failed task whose pane was kept for a person (login_required,
//     agent_rate_limited, agent_api_error) is held too and stays failed.
//     Any other ended task is ErrTaskInactive.
//
// The hold and the status change are written together as a
// compare-and-set on the status read (LOOM-262): if the task or its hold
// changed meanwhile, Takeover looks again, and gives up with
// ErrStatusChanged if that keeps happening. A turn in flight on a
// running task isn't interrupted here: its own status write then loses
// its compare-and-set and it ends leaving the task as it is.
//
// registry.ErrNotFound for an unknown task, ErrUnsupportedHoldMode for
// watch, *InvalidTakeoverError for an unknown mode or a TTL out of
// bounds.
func (o *Orchestrator) Takeover(ctx context.Context, req TakeoverRequest) (*TakeoverResult, error) {
	switch req.Mode {
	case "", registry.HoldModeDrive:
	case registry.HoldModeWatch:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedHoldMode, req.Mode)
	default:
		return nil, &InvalidTakeoverError{Reason: fmt.Sprintf("mode must be %q or %q", registry.HoldModeDrive, registry.HoldModeWatch)}
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultHoldTTL
	}
	if ttl < MinHoldTTL || ttl > MaxHoldTTL {
		return nil, &InvalidTakeoverError{Reason: fmt.Sprintf("the lease must be %d to %d seconds",
			int(MinHoldTTL.Seconds()), int(MaxHoldTTL.Seconds()))}
	}
	if req.SessionID == "" {
		return nil, &InvalidTakeoverError{Reason: "a hold needs a session"}
	}

	for range takeoverAttempts {
		task, err := o.store.GetTask(ctx, req.TaskID)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: takeover: %w", err)
		}
		var replace registry.HoldEndReason
		held, err := o.store.GetActiveDriveHold(ctx, task.ID)
		switch {
		case err == nil && held.SessionID == req.SessionID:
			renewed, err := o.store.RenewTaskHold(ctx, held.ID, o.now().UTC())
			if errors.Is(err, registry.ErrHoldEnded) {
				continue // ended since it was read: look again
			}
			if err != nil {
				return nil, fmt.Errorf("orchestrator: takeover: %w", err)
			}
			return &TakeoverResult{Hold: renewed, Task: task, Renewed: true}, nil
		case err == nil:
			// Another session's. If the task ended while it was held,
			// say so rather than "held": forcing wouldn't get it either.
			if _, err := heldStatus(task); err != nil {
				return nil, err
			}
			if !req.Force {
				return nil, &TaskHeldError{Hold: held}
			}
			replace = registry.HoldEndTakenOver
		case !errors.Is(err, registry.ErrNotFound):
			return nil, fmt.Errorf("orchestrator: takeover: %w", err)
		}

		from := task.Status
		to, err := heldStatus(task)
		if err != nil {
			return nil, err
		}
		hold := &registry.TaskHold{
			ID:          uuid.NewString(),
			TaskID:      task.ID,
			SessionID:   req.SessionID,
			ClientKind:  req.ClientKind,
			ClientLabel: req.ClientLabel,
			Mode:        registry.HoldModeDrive,
			PrevStatus:  from,
			TTL:         ttl,
			AcquiredAt:  o.now().UTC(),
		}
		task.Status = to
		replaced, applied, err := o.store.AcquireDriveHold(ctx, hold, task, from, replace)
		if errors.Is(err, registry.ErrTaskHeld) {
			continue // another session took it since the hold was looked for
		}
		if err != nil {
			return nil, fmt.Errorf("orchestrator: takeover: %w", err)
		}
		if !applied {
			continue // the status changed since it was read
		}
		if from != to {
			o.recordTaskTransition(string(from), string(to), task.Kind)
		}
		attrs := []any{"task_id", task.ID, "hold_id", hold.ID, "session_id", hold.SessionID,
			"client_label", hold.ClientLabel, "prev_status", string(from)}
		if replaced != nil {
			attrs = append(attrs, "replaced_hold_id", replaced.ID, "replaced_session_id", replaced.SessionID)
		}
		o.logger.Info("task taken over", attrs...)
		return &TakeoverResult{Hold: hold, Task: task, Replaced: replaced}, nil
	}
	return nil, fmt.Errorf("orchestrator: takeover: %w: task %q kept changing", ErrStatusChanged, req.TaskID)
}

// heldStatus is the status task has once a drive hold is taken on it,
// or ErrTaskInactive if it can't be held.
func heldStatus(task *registry.Task) (registry.TaskStatus, error) {
	switch task.Status {
	case registry.TaskStatusRunning, registry.TaskStatusAwaitingInput, registry.TaskStatusNeedsAttention,
		registry.TaskStatusHumanTakeover:
		return registry.TaskStatusHumanTakeover, nil
	case registry.TaskStatusFailed:
		// Its pane was kept for a person to sign in or look: nothing
		// automated drives a failed task, so it stays failed while held.
		switch task.ErrorClass {
		case registry.ErrorClassLoginRequired, registry.ErrorClassAgentRateLimited, registry.ErrorClassAgentAPIError:
			return registry.TaskStatusFailed, nil
		}
	}
	return "", fmt.Errorf("%w: task %q is %s", ErrTaskInactive, task.ID, task.Status)
}

// Heartbeat renews sessionID's hold holdID on taskID, pushing its expiry
// out by its lease, and returns it. registry.ErrNotFound if there is no
// such hold on that task, ErrHoldNotOwned if it is another session's,
// *HoldEndedError (with the hold, saying why) if it has ended.
func (o *Orchestrator) Heartbeat(ctx context.Context, taskID, holdID, sessionID string) (*registry.TaskHold, error) {
	hold, err := o.store.GetTaskHold(ctx, holdID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: heartbeat: %w", err)
	}
	if hold.TaskID != taskID {
		return nil, fmt.Errorf("orchestrator: heartbeat: %w: hold %q on task %q", registry.ErrNotFound, holdID, taskID)
	}
	if hold.SessionID != sessionID {
		return nil, ErrHoldNotOwned
	}
	renewed, err := o.store.RenewTaskHold(ctx, holdID, o.now().UTC())
	if errors.Is(err, registry.ErrHoldEnded) {
		return nil, &HoldEndedError{Hold: renewed}
	}
	if err != nil {
		return nil, fmt.Errorf("orchestrator: heartbeat: %w", err)
	}
	return renewed, nil
}

// Release ends a hold and hands its task back to automated dispatch.
// With holdID it ends that hold as released; an already-ended hold comes
// back as it ended, so releasing twice is harmless. Without it, it is
// Take back: whatever drive hold the task has is ended as taken_back.
// Either way a task left in human-takeover with no drive hold becomes
// awaiting-input (ready for the next message, since nothing is in flight
// the moment control is handed back) and its stale prompt is cleared; a
// task in any other status keeps it, and a task another session has taken
// meanwhile stays theirs. The hold's end and the task's release are one
// write, so no takeover can land between them.
//
// registry.ErrNotFound for an unknown task, or a hold that isn't the
// task's.
func (o *Orchestrator) Release(ctx context.Context, taskID, holdID string) (*registry.HoldRelease, error) {
	reason := registry.HoldEndReleased
	if holdID == "" {
		reason = registry.HoldEndTakenBack
	}
	res, err := o.store.ReleaseTaskHold(ctx, taskID, holdID, reason, o.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("orchestrator: release: %w", err)
	}
	if res.TaskReleased {
		o.recordTaskTransition(string(registry.TaskStatusHumanTakeover), string(res.Task.Status), res.Task.Kind)
	}
	if res.Ended || res.TaskReleased {
		attrs := []any{"task_id", taskID, "task_released", res.TaskReleased}
		if res.Hold != nil {
			attrs = append(attrs, "hold_id", res.Hold.ID, "ended", res.Ended, "ended_reason", string(res.Hold.EndedReason))
		}
		o.logger.Info("task released", attrs...)
	}
	return res, nil
}
