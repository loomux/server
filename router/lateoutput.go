package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// RelayLateOutput relays what agents said after their turn was already
// relayed (LOOM-121), and returns how many such replies it relayed. An
// agent can end a turn early — "the build runs in the background, I'll
// report when it finishes" — and carry on once the job is done; its
// completion hook then saves a fresh marker and final message that no
// turn is waiting for. One pass looks at every task waiting for input
// whose agent-type signals completion with a marker, and relays a new
// marker's message to the conversation as a reply of its own, as a
// finished turn would (rolling summary, transcript, completion when the
// relay model calls it done). A conversation with a turn in flight is
// skipped: that turn owns the pane and clears an old marker before it
// types. Failures before the reply is logged consume nothing, so the
// next pass tries again.
//
// A task handed back from a human takeover to waiting for input counts
// too: a marker the agent wrote while the human drove it is relayed
// as a late reply, which is what the conversation would want to hear.
func (r *Router) RelayLateOutput(ctx context.Context) int {
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		r.logger.Error("late output pass failed", "error", err)
		return 0
	}
	relayed := 0
	for _, t := range tasks {
		if t.Kind != registry.TaskKindAgent || t.Status != registry.TaskStatusAwaitingInput {
			continue
		}
		if entry, err := r.agentTypes.Get(t.AgentType); err != nil || entry.Tier != completion.TierMarker {
			continue
		}
		ok, err := r.relayLateOutput(ctx, t)
		// An unreachable target is the health probe's to report; this
		// pass runs too often to log it each time.
		if err != nil && !errors.Is(err, targets.ErrUnreachable) {
			r.logger.Warn("late agent output not relayed", "task_id", t.ID,
				"conversation_id", t.ConversationID, "error", err)
		}
		if ok {
			relayed++
		}
	}
	return relayed
}

// relayLateOutput relays task's late reply, if its agent has written one;
// whether it did.
func (r *Router) relayLateOutput(ctx context.Context, task *registry.Task) (bool, error) {
	unlock := r.conversations.tryLock(task.ConversationID)
	if unlock == nil {
		return false, nil
	}
	defer unlock()
	// Re-read under the lock: a turn may have moved the task on since
	// the list was taken.
	task, err := r.store.GetTask(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if task.Status != registry.TaskStatusAwaitingInput {
		return false, nil
	}
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return false, err
	}
	marker, err := r.markerPathOn(ctx, exec, task.ID)
	if err != nil {
		return false, err
	}
	exists, err := exec.FileExists(ctx, marker)
	if err != nil || !exists {
		return false, err
	}
	start := time.Now()
	log := r.logger.With("task_id", task.ID, "conversation_id", task.ConversationID)
	log.Info("agent wrote output after its turn ended; relaying it")

	// Nothing is consumed until the reply is logged: a relay that fails
	// leaves the marker and the agent's message for the next pass.
	captured, agentMessage, err := r.turnOutput(ctx, exec, task, true)
	if err != nil {
		return false, fmt.Errorf("capture pane: %w", err)
	}
	result, err := r.model.Relay(ctx, captured)
	if err != nil {
		return false, fmt.Errorf("relay: %w", err)
	}
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: task.ConversationID,
		TaskID:         task.ID,
		Role:           registry.MessageRoleAssistant,
		Content:        result.Reply,
	}); err != nil {
		return false, fmt.Errorf("log reply: %w", err)
	}
	// Logged: from here a failure must not relay it again.
	r.clearTurnFiles(ctx, exec, task)
	r.recordTurn(ctx, exec, task, "", agentMessage)
	if result.Done && AsksUser(result.Reply) {
		result.Done = false
	}
	if result.Done {
		if err := r.orch.Complete(ctx, task.ID, result.Reply); err != nil {
			log.Error("late reply relayed, but its task not completed", "error", err)
		}
	} else if err := r.store.SetWorkspaceRollingSummary(ctx, task.WorkspaceID, result.Reply); err != nil {
		log.Error("late reply relayed, but the rolling summary not updated", "error", err)
	}
	if r.onLateReply != nil {
		r.onLateReply(task, result.Reply)
	}
	log.Info("late agent output relayed", "task_done", result.Done,
		"duration_ms", time.Since(start).Milliseconds())
	return true, nil
}
