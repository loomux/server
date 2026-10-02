package orchestrator

import (
	"context"
	"fmt"

	"github.com/Loomux/server/registry"
)

// Takeover flips a task to human-takeover, pausing automated SendMessage
// dispatch for it until Release is called. Refuses on a task that's
// already completed or failed.
func (o *Orchestrator) Takeover(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: takeover: %w", err)
	}
	if task.Status == registry.TaskStatusCompleted || task.Status == registry.TaskStatusFailed {
		return ErrTaskInactive
	}
	prevStatus := task.Status
	task.Status = registry.TaskStatusHumanTakeover
	o.recordTaskTransition(string(prevStatus), string(task.Status), task.Kind)
	if err := o.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("orchestrator: takeover: %w", err)
	}
	return nil
}

// Release hands a task back to automated dispatch after a human takeover.
// The task becomes AwaitingInput — ready for the next message, since
// nothing is actively in flight the moment control is handed back.
func (o *Orchestrator) Release(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: release: %w", err)
	}
	if task.Status == registry.TaskStatusCompleted || task.Status == registry.TaskStatusFailed {
		return ErrTaskInactive
	}
	prevStatus := task.Status
	task.Status = registry.TaskStatusAwaitingInput
	o.recordTaskTransition(string(prevStatus), string(task.Status), task.Kind)
	if err := o.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("orchestrator: release: %w", err)
	}
	return nil
}
