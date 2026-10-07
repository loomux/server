package router

import (
	"context"

	"github.com/Loomux/server/registry"
)

// What the router models — third-party APIs — may see of a target's work
// (TargetPolicy.Relay, user decision 2026-10-07): everything (full), only
// the agent's final message for each turn (last_message), or nothing
// (none, the default for a work machine). The same policy gates the
// relay model's input for the target's turns and the routing model's
// view of the history, open task and summaries those turns left. The
// message being routed always reaches the routing model: routing needs it.

// noRelayReplyRunes bounds a reply that is the agent's own message, sent
// to no model (RelayNone).
const noRelayReplyRunes = 4000

// turnOriginTarget is the target a message the turn logs about taskID
// acted on: the task's target, or failing a task, the target the turn
// acts on; "" for none or unknown.
func (r *Router) turnOriginTarget(ctx context.Context, taskID string) string {
	if taskID != "" {
		if task, err := r.store.GetTask(ctx, taskID); err == nil {
			if ws, err := r.store.GetWorkspace(ctx, task.WorkspaceID); err == nil {
				return ws.TargetID
			}
		}
		return ""
	}
	return turnLogFrom(ctx).originTarget
}

// taskRelayPolicy is the relay policy of task's target. A target that
// can't be read sends nothing: the policy fails closed.
func (r *Router) taskRelayPolicy(ctx context.Context, task *registry.Task) string {
	ws, err := r.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return registry.RelayNone
	}
	t, err := r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return registry.RelayNone
	}
	return t.Policy.EffectiveRelay()
}

// relayTurn condenses a finished turn under its target's relay policy:
// captured is what turnOutput read (the agent's final message, or its
// screen), agentMessage that message ("" with no completion hook);
// message is the user's ("" for late output).
func (r *Router) relayTurn(ctx context.Context, task *registry.Task, message, captured, agentMessage string) (RelayResult, string, error) {
	policy := r.taskRelayPolicy(ctx, task)
	final := agentMessage
	if final == "" {
		final = captured
	}
	switch policy {
	case registry.RelayNone:
		// No model: the agent's own words are the reply, redacted and
		// bounded. The task stays open, as the comment in awaitTurn
		// explains: wrongly keeping a session costs an idle pane until the
		// reaper, wrongly closing one loses it.
		reply := keepEnd(r.redactAllSecrets(ctx, final), noRelayReplyRunes)
		if reply == "" {
			reply = "(the agent finished its turn without a message)"
		}
		return RelayResult{Reply: reply}, policy, nil
	case registry.RelayLastMessage:
		res, err := r.model.Relay(ctx, RelayInput{AgentType: task.AgentType, Captured: r.scrubForRelay(ctx, final)})
		return res, policy, err
	default:
		res, err := r.model.Relay(ctx, r.relayInput(ctx, task, message, captured))
		return res, policy, err
	}
}

// historyPolicies resolves, per message, the relay policy of the target
// its turn acted on, for what the routing model may see of it: from the
// message's recorded target; for older ones, their task's target, or a
// message of the same dispatch that has one; failing all, its origin's
// default (a work machine: none). A turn that touched no target is full.
type historyPolicies struct {
	r       *Router
	ctx     context.Context
	targets map[string]string // target id → policy ("" unknown)
	tasks   map[string]string // task id → target id
}

func (r *Router) newHistoryPolicies(ctx context.Context) *historyPolicies {
	return &historyPolicies{r: r, ctx: ctx, targets: map[string]string{}, tasks: map[string]string{}}
}

func (h *historyPolicies) target(id string) string {
	if p, ok := h.targets[id]; ok {
		return p
	}
	p := ""
	if t, err := h.r.store.GetTarget(h.ctx, id); err == nil {
		p = t.Policy.EffectiveRelay()
	}
	h.targets[id] = p
	return p
}

func (h *historyPolicies) taskTarget(taskID string) string {
	if id, ok := h.tasks[taskID]; ok {
		return id
	}
	id := ""
	if task, err := h.r.store.GetTask(h.ctx, taskID); err == nil {
		if ws, err := h.r.store.GetWorkspace(h.ctx, task.WorkspaceID); err == nil {
			id = ws.TargetID
		}
	}
	h.tasks[taskID] = id
	return id
}

// filter keeps what the routing model may see of msgs: under none,
// nothing of the turn; under last_message, its replies only.
func (h *historyPolicies) filter(msgs []*registry.Message) []*registry.Message {
	dispatchTarget := map[string]string{}
	dispatchOrigin := map[string]string{}
	for _, m := range msgs {
		if m.DispatchID == "" {
			continue
		}
		if t := h.messageTarget(m); t != "" {
			dispatchTarget[m.DispatchID] = t
		}
		if m.Origin != "" {
			dispatchOrigin[m.DispatchID] = m.Origin
		}
	}
	out := msgs[:0:0]
	for _, m := range msgs {
		target, origin := h.messageTarget(m), m.Origin
		if target == "" && m.DispatchID != "" {
			target = dispatchTarget[m.DispatchID]
		}
		if origin == "" && m.DispatchID != "" {
			origin = dispatchOrigin[m.DispatchID]
		}
		policy := ""
		if target != "" {
			policy = h.target(target)
		}
		if policy == "" {
			policy = registry.TargetPolicy{Purpose: origin}.EffectiveRelay()
		}
		switch policy {
		case registry.RelayNone:
			continue
		case registry.RelayLastMessage:
			if m.Role != registry.MessageRoleAssistant {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func (h *historyPolicies) messageTarget(m *registry.Message) string {
	if m.OriginTargetID != "" {
		return m.OriginTargetID
	}
	if m.TaskID != "" {
		return h.taskTarget(m.TaskID)
	}
	return ""
}
