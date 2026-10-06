package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// bootRetry and bootRetryFor pace startup reconciliation's retries of a
// target it can't reach yet: right after a restart the network sidecar
// may not be up, which says nothing about the target (LOOM-82).
var (
	bootRetry    = 5 * time.Second
	bootRetryFor = 2 * time.Minute
)

// restartLostReason is why a task whose session vanished while loomuxd
// was down is failed (LOOM-82).
const restartLostReason = "its session was gone after loomuxd restarted"

// ResumeDispatch is how a dispatch job a previous loomuxd left running
// carries on (LOOM-82): when its conversation has an agent task still
// running, the returned func waits on that agent's turn — waiting is
// stateless, a marker or an idle pane either way — relays it and records
// the reply, as the interrupted dispatch would have. nil when there is
// no such task (the job was still routing or provisioning) and nothing
// to resume. taskID is the task it waits on, for ReconcileTasks to
// leave alone.
func (r *Router) ResumeDispatch(ctx context.Context, d *registry.Dispatch) (taskID string,
	run func(context.Context, *registry.Dispatch) (string, error)) {
	task, err := r.runningAgentTask(ctx, d.ConversationID)
	if err != nil {
		r.logger.Error("dispatch not resumed", "dispatch_id", d.ID, "error", err)
		return "", nil
	}
	if task == nil {
		return "", nil
	}
	return task.ID, func(ctx context.Context, d *registry.Dispatch) (reply string, err error) {
		defer r.conversations.lock(d.ConversationID)()
		ctx = withTurnLog(ctx, turnLog{dispatchID: d.ID, userMessageLogged: true})
		log := r.logger.With("conversation_id", d.ConversationID, "dispatch_id", d.ID, "workspace_id", task.WorkspaceID,
			"task_id", task.ID, "agent_type", task.AgentType)
		failClass := registry.ErrorClassInternal
		defer func() { r.failTurnOnError(ctx, log, task.ID, failClass, err) }()

		live, err := r.sessionIsLiveAtBoot(ctx, task)
		if err != nil {
			return "", fmt.Errorf("router: resume: %w", err)
		}
		if !live {
			if err := r.orch.Fail(ctx, task.ID, registry.TaskFailure{
				Class: registry.ErrorClassSessionLost, Reason: restartLostReason,
			}); err != nil {
				return "", fmt.Errorf("router: resume: %w", err)
			}
			return "", fmt.Errorf("router: resume: the %s agent's session was gone after loomuxd restarted, so its reply is lost; "+
				"send your message again", task.AgentType)
		}
		log.Info("agent dispatch resumed after restart")
		return r.awaitTurn(ctx, log, task, d.Message, time.Now(), &failClass)
	}
}

// runningAgentTask is conversationID's agent task that is mid-turn, if any.
func (r *Router) runningAgentTask(ctx context.Context, conversationID string) (*registry.Task, error) {
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	var found *registry.Task
	for _, t := range tasks {
		if t.ConversationID == conversationID && t.Kind == registry.TaskKindAgent && t.Status == registry.TaskStatusRunning &&
			(found == nil || t.UpdatedAt.After(found.UpdatedAt)) {
			found = t
		}
	}
	return found, nil
}

// ReconcileTasks settles, at startup, the tasks a previous loomuxd left
// mid-turn that no resumed dispatch is waiting on (LOOM-82): one whose
// session is gone is failed — nothing will ever finish it — and a
// command whose process exited meanwhile is finished with its exit
// code. A live agent with nobody waiting is left to the next message.
// skip holds the tasks resumed dispatches have taken. Errors are logged
// per task: one unreachable target mustn't stop the rest.
func (r *Router) ReconcileTasks(ctx context.Context, skip map[string]bool) {
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		r.logger.Error("startup task reconciliation failed", "error", err)
		return
	}
	var pending []*registry.Task
	for _, t := range tasks {
		if t.Status == registry.TaskStatusRunning && !skip[t.ID] {
			pending = append(pending, t)
		}
	}
	// A target that can't be reached yet is retried until bootRetryFor.
	deadline := time.Now().Add(bootRetryFor)
	for len(pending) > 0 {
		var retry []*registry.Task
		for _, t := range pending {
			if errors.Is(r.reconcileTask(ctx, t), targets.ErrUnreachable) {
				retry = append(retry, t)
			}
		}
		if len(retry) == 0 || time.Now().After(deadline) {
			for _, t := range retry {
				r.logger.Warn("task not reconciled: its target stayed unreachable", "task_id", t.ID)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(bootRetry):
		}
		pending = retry
	}
}

// reconcileTask settles one task for ReconcileTasks. It returns an
// error only when the target couldn't be reached.
func (r *Router) reconcileTask(ctx context.Context, t *registry.Task) error {
	log := r.logger.With("task_id", t.ID, "workspace_id", t.WorkspaceID, "task_kind", string(t.Kind))
	exec, err := r.executorFor(ctx, t)
	if err != nil {
		log.Warn("task not reconciled", "error", err)
		return nil
	}
	live, err := exec.HasSession(ctx, t.TmuxSession)
	if err != nil {
		return err
	}
	if !live {
		if err := r.orch.Fail(ctx, t.ID, registry.TaskFailure{Class: registry.ErrorClassSessionLost, Reason: restartLostReason}); err != nil {
			log.Error("task not reconciled", "error", err)
			return nil
		}
		log.Info("failed a task whose session was gone after the restart")
		return nil
	}
	if t.Kind != registry.TaskKindCommand {
		log.Info("left a live agent task for the next message")
		return nil
	}
	exit, err := exec.PaneExited(ctx, t.TmuxSession)
	if err != nil || exit == nil {
		return nil
	}
	if err := r.orch.FinishCommand(ctx, t.ID, exit.Status); err != nil {
		log.Error("task not reconciled", "error", err)
		return nil
	}
	log.Info("finished a command that exited while loomuxd was down", "exit_code", exit.Status)
	return nil
}

// sessionIsLiveAtBoot is sessionIsLive, retrying a target that can't be
// reached yet (see bootRetry).
func (r *Router) sessionIsLiveAtBoot(ctx context.Context, task *registry.Task) (bool, error) {
	deadline := time.Now().Add(bootRetryFor)
	for {
		live, err := r.sessionIsLive(ctx, task)
		if !errors.Is(err, targets.ErrUnreachable) || time.Now().After(deadline) {
			return live, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(bootRetry):
		}
	}
}
