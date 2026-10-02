// Package orchestrator wires the workspace registry (registry.Store) and
// TargetExecutor together into task lifecycle management: launching a
// task, sending follow-up input, tearing a task down, and the
// takeover/release handshake. See design spec §3, §4.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// ErrHumanTakeover is returned when an operation that would collide with
// an active human takeover (e.g. SendMessage) is attempted.
var ErrHumanTakeover = errors.New("orchestrator: task is under human takeover")

// ErrTaskInactive is returned when an operation is attempted against a
// task that has already completed or failed.
var ErrTaskInactive = errors.New("orchestrator: task is not active")

// ProcessExitedError is what a CompletionDetector returns when the
// process a task's pane was running exits while it was waiting for a
// turn to complete (LOOM-71) — an agent CLI that isn't installed, or one
// that crashed. Output is the pane's final output (targets.PaneExit), so
// the failure can be reported in the process's own words ("codex:
// command not found") rather than as a missing pane. A TaskKindCommand
// task never gets this: its process exiting is its completion.
type ProcessExitedError struct {
	// Status is the exit status, -1 if the process was killed by a
	// signal.
	Status int
	Output string
}

func (e *ProcessExitedError) Error() string {
	return fmt.Sprintf("process exited with status %d", e.Status)
}

// TimeoutReason says which bound a timed-out turn hit (LOOM-76).
type TimeoutReason string

const (
	// TimeoutMaxTurnDuration: the turn ran longer than its agent-type's
	// maximum.
	TimeoutMaxTurnDuration TimeoutReason = "max_turn_duration"
	// TimeoutNoProgress: the agent's pane didn't change for its
	// agent-type's no-progress timeout while no completion signal came —
	// most likely blocked on a prompt.
	TimeoutNoProgress TimeoutReason = "no_progress"
)

// TurnTimeoutError is what a CompletionDetector returns when a turn hits
// one of its bounds (LOOM-76) rather than completing. The task's pane is
// still there: nothing about a timeout tears it down.
type TurnTimeoutError struct {
	Reason TimeoutReason
	Limit  time.Duration
}

func (e *TurnTimeoutError) Error() string {
	if e.Reason == TimeoutNoProgress {
		return fmt.Sprintf("no progress for %s with no completion signal (likely waiting on a prompt)", e.Limit)
	}
	return fmt.Sprintf("the turn ran past its %s limit", e.Limit)
}

// CompletionDetector learns when a task's current turn is done. The real
// tiered strategy (native hooks / self-report / idle heuristic, design
// spec §5) is a separate concern (LOOM-6) — this seam exists so
// orchestrator internals never need to know which tier is active.
type CompletionDetector interface {
	// Wait blocks until task's current turn is judged complete, or ctx
	// is done, whichever comes first. If the pane's process exits first
	// it returns a *ProcessExitedError (except for a TaskKindCommand
	// task, whose exit is its completion).
	Wait(ctx context.Context, task *registry.Task) error
}

// ExecutorFactory constructs the TargetExecutor for a target. Abstracted
// out (rather than calling targets.NewExecutor directly) so tests can
// inject a fake in-memory executor.
type ExecutorFactory func(*registry.Target) (targets.TargetExecutor, error)

// Orchestrator manages task lifecycle against a registry.Store, a
// TargetExecutor (via ExecutorFactory), and a CompletionDetector.
type Orchestrator struct {
	store       registry.Store
	newExecutor ExecutorFactory
	detector    CompletionDetector
}

// New constructs an Orchestrator.
func New(store registry.Store, newExecutor ExecutorFactory, detector CompletionDetector) *Orchestrator {
	return &Orchestrator{store: store, newExecutor: newExecutor, detector: detector}
}
