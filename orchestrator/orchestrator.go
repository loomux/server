// Package orchestrator wires the workspace registry (registry.Store) and
// TargetExecutor together into task lifecycle management: launching a
// task, sending follow-up input, tearing a task down, and the
// takeover/release handshake. See design spec §3, §4.
package orchestrator

import (
	"context"
	"errors"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// ErrHumanTakeover is returned when an operation that would collide with
// an active human takeover (e.g. SendMessage) is attempted.
var ErrHumanTakeover = errors.New("orchestrator: task is under human takeover")

// ErrTaskInactive is returned when an operation is attempted against a
// task that has already completed or failed.
var ErrTaskInactive = errors.New("orchestrator: task is not active")

// CompletionDetector learns when a task's current turn is done. The real
// tiered strategy (native hooks / self-report / idle heuristic, design
// spec §5) is a separate concern (LOOM-6) — this seam exists so
// orchestrator internals never need to know which tier is active.
type CompletionDetector interface {
	// Wait blocks until task's current turn is judged complete, or ctx
	// is done, whichever comes first.
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
