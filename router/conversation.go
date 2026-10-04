package router

import (
	"context"
	"fmt"

	"github.com/Loomux/server/registry"
)

// conversationContext gathers what the routing model should know about
// the conversation a message continues (LOOM-87): its recent turns, the
// task still waiting on the user if there is one, and otherwise the
// workspace it last worked in. offered is the workspace snapshot Decide
// will see: an open task or last workspace outside it (failed, archived,
// shell) isn't passed.
func (r *Router) conversationContext(ctx context.Context, conversationID string, offered []WorkspaceSnapshot) (
	history []ConversationTurn, open *OpenTaskSnapshot, lastWorkspaceID, lastWorkspaceName string, err error) {
	msgs, err := r.store.ListMessagesByConversation(ctx, conversationID)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("conversation history: %w", err)
	}
	// Under a dispatch job the message being routed is already stored
	// (LOOM-80); it is the message, not history.
	current := turnLogFrom(ctx).dispatchID
	kept := msgs[:0:0]
	for _, m := range msgs {
		if current != "" && m.DispatchID == current {
			continue
		}
		kept = append(kept, m)
	}
	history = boundHistory(kept)

	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("conversation tasks: %w", err)
	}
	names := make(map[string]string, len(offered))
	for _, ws := range offered {
		names[ws.ID] = ws.Name
	}
	var latest, awaiting *registry.Task
	for _, t := range tasks {
		if t.ConversationID != conversationID {
			continue
		}
		if _, ok := names[t.WorkspaceID]; !ok {
			continue
		}
		if latest == nil || t.UpdatedAt.After(latest.UpdatedAt) {
			latest = t
		}
		if t.Kind == registry.TaskKindAgent && t.Status == registry.TaskStatusAwaitingInput &&
			(awaiting == nil || t.UpdatedAt.After(awaiting.UpdatedAt)) {
			awaiting = t
		}
	}
	if awaiting != nil {
		open = &OpenTaskSnapshot{
			TaskID:        awaiting.ID,
			WorkspaceID:   awaiting.WorkspaceID,
			WorkspaceName: names[awaiting.WorkspaceID],
			AgentType:     awaiting.AgentType,
			Status:        string(awaiting.Status),
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].TaskID == awaiting.ID && msgs[i].Role == registry.MessageRoleAssistant {
				open.LastReply = keepEnd(msgs[i].Content, OpenTaskReplyRunes)
				break
			}
		}
		return history, open, "", "", nil
	}
	if latest != nil {
		return history, nil, latest.WorkspaceID, names[latest.WorkspaceID], nil
	}
	return history, nil, "", "", nil
}

// boundHistory keeps the last HistoryMessages messages, each cut — a
// user message keeps its start (what was asked), a reply its end (where
// a question to the user usually is) — and drops the oldest until the
// whole fits in HistoryRunes.
func boundHistory(msgs []*registry.Message) []ConversationTurn {
	if len(msgs) > HistoryMessages {
		msgs = msgs[len(msgs)-HistoryMessages:]
	}
	turns := make([]ConversationTurn, 0, len(msgs))
	total := 0
	for _, m := range msgs {
		content := truncateRunes(m.Content, HistoryMessageRunes)
		if m.Role == registry.MessageRoleAssistant {
			content = keepEnd(m.Content, HistoryMessageRunes)
		}
		turns = append(turns, ConversationTurn{Role: string(m.Role), Content: content})
		total += len([]rune(content))
	}
	for len(turns) > 1 && total > HistoryRunes {
		total -= len([]rune(turns[0].Content))
		turns = turns[1:]
	}
	return turns
}

// keepEnd keeps the last n runes of s, marking a cut with a leading "…".
func keepEnd(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// ApplyAffinity is the deterministic half of conversation affinity
// (LOOM-87): with a task open in the conversation, a decision that
// doesn't continue it is overridden to — unless the model said the
// message is unrelated (LeaveOpenTask). Continuing it always uses the
// open task's agent: the turn goes into the pane that's there.
// overriddenFrom names the action that was replaced, if any. Exported
// so the routing evals (llmrouter, build tag routereval) score a
// decision as the router acts on it.
func ApplyAffinity(d Decision, open *OpenTaskSnapshot) (out Decision, overriddenFrom string) {
	if open == nil {
		return d, ""
	}
	continues := d.Action == ActionUseWorkspace && d.WorkspaceID == open.WorkspaceID
	// A command or a new workspace is a deliberate choice of something
	// else: leaving the task as surely as leave_open_task. What gets
	// overridden is the router answering a follow-up itself, or sending
	// it to another workspace — the failures affinity exists for.
	leaves := d.LeaveOpenTask || d.Action == ActionRunCommand || d.Action == ActionProvisionWorkspace
	if !continues && !leaves {
		overriddenFrom = string(d.Action)
		d = Decision{Action: ActionUseWorkspace, WorkspaceID: open.WorkspaceID}
		continues = true
	}
	if continues {
		d.AgentType = open.AgentType
	}
	return d, overriddenFrom
}
