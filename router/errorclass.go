package router

import (
	"context"
	"errors"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// ClassifyError maps a failed Dispatch's error to the class a dispatch
// job records (LOOM-80): the turn-level classes a client can act on
// (timeout, agent exited) where the error carries them, otherwise the
// same classes the dispatch metrics use.
func ClassifyError(err error) registry.ErrorClass {
	// First: a turn whose task was taken over (LOOM-266) ends so,
	// whatever it was doing when the takeover landed.
	if errors.Is(err, orchestrator.ErrTakenOver) {
		return registry.ErrorClassTakenOver
	}
	var classed *classedError
	if errors.As(err, &classed) {
		return classed.class
	}
	var timeout *orchestrator.TurnTimeoutError
	if errors.As(err, &timeout) {
		return registry.ErrorClassTimeout
	}
	var exited *orchestrator.ProcessExitedError
	if errors.As(err, &exited) {
		return registry.ErrorClassAgentExited
	}
	var compaction *orchestrator.CompactionLoopError
	if errors.As(err, &compaction) {
		return registry.ErrorClassCompactionLoop
	}
	if c := classifyDispatchError(err); c != "" {
		return registry.ErrorClass(c)
	}
	return registry.ErrorClassInternal
}

// classifyDispatchError maps a dispatch error to a stable error_class
// label for metrics, reusing registry.ErrorClass values from LOOM-77.
// It keeps cardinality bounded by never using the error message as a
// label. The empty string means "no error" (success).
func classifyDispatchError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, orchestrator.ErrTakenOver) {
		return string(registry.ErrorClassTakenOver)
	}
	var classed *classedError
	if errors.As(err, &classed) {
		return string(classed.class)
	}
	if errors.Is(err, targets.ErrTextTooLarge) {
		return string(registry.ErrorClassMessageTooLarge)
	}
	if errors.Is(err, targets.ErrUnreachable) {
		return string(registry.ErrorClassTargetUnreachable)
	}
	var unhealthy *TargetUnhealthyError
	if errors.As(err, &unhealthy) {
		return string(registry.ErrorClassTargetUnhealthy)
	}
	if errors.Is(err, orchestrator.ErrHumanTakeover) {
		// A person holds the task (LOOM-267): not a fault, and a client
		// shows who instead (the error names them, see HeldError).
		return string(registry.ErrorClassHeld)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return string(registry.ErrorClassWaitFailed)
	}
	return string(registry.ErrorClassInternal)
}

// classedError is a dispatch error that carries the class its task
// failed with, for the job and the metrics: an agent that isn't signed
// in, or is at its usage limit (LOOM-109), which a client words
// differently from an internal error.
type classedError struct {
	class registry.ErrorClass
	msg   string
}

func (e *classedError) Error() string { return e.msg }

// wrappedError is msg, which already says what err does: errors.As and
// errors.Is still find err, but its text isn't repeated.
type wrappedError struct {
	msg string
	err error
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.err }

// HeldError is a message refused because a person holds the
// conversation's task (LOOM-267): it is driven from a terminal (or
// another client), and automation doesn't type into it until it is
// given or taken back. Its text names the holder, for the dispatch's
// error; it wraps orchestrator.ErrHumanTakeover, so its class is held.
type HeldError struct {
	// Hold is the task's active drive hold; nil when there is none to
	// name (the task was taken before holds existed, or was just given
	// back).
	Hold *registry.TaskHold
	err  error
}

func (e *HeldError) Error() string {
	const next = "; take it back, or wait until it is given back, then send the message again"
	if e.Hold == nil {
		return "a person has taken over this conversation's task" + next
	}
	holder := "a client"
	switch e.Hold.ClientKind {
	case registry.SessionKindTUI:
		holder = "the terminal"
	case "web":
		holder = "the web"
	case "app":
		holder = "the app"
	}
	if e.Hold.ClientLabel != "" {
		holder += " on " + e.Hold.ClientLabel
	}
	return "this conversation's task is being driven from " + holder +
		" since " + e.Hold.AcquiredAt.UTC().Format("2006-01-02 15:04 UTC") + next
}

func (e *HeldError) Unwrap() error { return e.err }

// heldError is err, a dispatch on conversationID refused because a
// person holds its task, as a HeldError naming who: the active drive
// hold on one of the conversation's tasks taken over. A hold that can't
// be read leaves it unnamed; the refusal stands either way.
func (r *Router) heldError(ctx context.Context, conversationID string, err error) error {
	held := &HeldError{err: err}
	tasks, lerr := r.store.ListTasksByConversation(ctx, conversationID)
	if lerr != nil {
		return held
	}
	for _, t := range tasks {
		if t.Status != registry.TaskStatusHumanTakeover {
			continue
		}
		if h, herr := r.store.GetActiveDriveHold(ctx, t.ID); herr == nil {
			held.Hold = h
			break
		}
	}
	return held
}
