package router

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// recordEvent appends e to its conversation's audit trail (LOOM-110),
// stamping its id, its dispatch from ctx, and conversationID. The trail
// is a record, never a reason for a turn to fail: a write that fails is
// logged and dropped.
func (r *Router) recordEvent(ctx context.Context, conversationID string, e registry.DispatchEvent) {
	e.ID = uuid.NewString()
	e.ConversationID = conversationID
	e.DispatchID = turnLogFrom(ctx).dispatchID
	if err := r.store.CreateDispatchEvent(context.WithoutCancel(ctx), &e); err != nil {
		r.logger.Error("dispatch event not recorded", "conversation_id", conversationID, "kind", e.Kind, "error", err)
	}
}

// recordDecision records the routing model's decision: its action, the
// model and tier that made it, and what it named. A direct answer's text
// isn't kept (it is chat content, already in the transcript).
func (r *Router) recordDecision(ctx context.Context, conversationID string, d Decision, took time.Duration, overrides string) {
	e := registry.DispatchEvent{Kind: registry.EventDecision, Model: d.Model, Tier: d.Tier,
		Outcome: string(d.Action), Duration: took, Detail: overrides}
	switch d.Action {
	case ActionUseWorkspace:
		e.WorkspaceID = d.WorkspaceID
		e.Detail = joinDetail("agent "+d.AgentType, overrides)
	case ActionProvisionWorkspace:
		e.TargetID = d.NewWorkspace.TargetID
		e.Detail = joinDetail("agent "+d.AgentType+", new workspace "+d.NewWorkspace.Name, overrides)
	case ActionRunCommand:
		e.TargetID = d.TargetID
		e.Command = r.redactAllSecrets(ctx, d.Command)
	}
	r.recordEvent(ctx, conversationID, e)
}

// recordCommand records a command Loomux ran in workspaceID (kind
// EventCommand or EventProvision): where, the command redacted, and how
// it ended.
func (r *Router) recordCommand(ctx context.Context, conversationID, kind, workspaceID, command string,
	res commandResult, err error, took time.Duration) {
	e := registry.DispatchEvent{Kind: kind, WorkspaceID: workspaceID, TaskID: res.taskID,
		Command: r.redactAllSecrets(ctx, command), Duration: took}
	if ws, gerr := r.store.GetWorkspace(ctx, workspaceID); gerr == nil {
		e.TargetID = ws.TargetID
	}
	// An error's text isn't kept: it can carry what the command printed.
	var running *stillRunningError
	switch {
	case errors.As(err, &running):
		e.Outcome = "still running"
		e.ErrorClass = registry.ErrorClassTimeout
	case err != nil:
		e.Outcome = "not finished"
		e.ErrorClass = ClassifyError(err)
	default:
		e.Outcome = res.ended
	}
	r.recordEvent(ctx, conversationID, e)
}

func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}
