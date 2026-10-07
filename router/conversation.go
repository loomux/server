package router

import (
	"context"
	"fmt"
	"strings"

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
	// What a target's relay policy lets the routing model see of it.
	history = boundHistory(r.newHistoryPolicies(ctx).filter(kept))

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
		for i := len(msgs) - 1; i >= 0 && r.taskRelayPolicy(ctx, awaiting) != registry.RelayNone; i-- {
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
		d = Decision{Action: ActionUseWorkspace, WorkspaceID: open.WorkspaceID, Model: d.Model, Tier: d.Tier}
		continues = true
	}
	if continues {
		d.AgentType = open.AgentType
	}
	return d, overriddenFrom
}

// earlierConversation is the note a freshly started agent gets in front
// of message: the conversation so far, bounded as for the routing model
// (boundHistory), or "" when there is none. The turn's own messages aren't
// "earlier"; nor, for a confirmed request carried out now (carriesOut),
// is the request itself or the offer and answer about it that followed.
// A message merely repeating an earlier one ("continue") cuts nothing.
//
// Only turns that ran on a target of the same purpose as workspaceID's
// are kept (command-center decision, 2026-10-05): work context, such as
// sc1's, never reaches a personal machine's agent, nor the reverse.
// Where a turn ran is its messages' Origin, recorded when it was logged,
// so a deleted workspace's turns keep theirs. Turns that touched no
// target (the router's own answers) are kept; a turn of unknown origin
// is left out, and if the agent's own target can't be read, there is no
// note at all.
func (r *Router) earlierConversation(ctx context.Context, conversationID, workspaceID, message string) (string, error) {
	msgs, err := r.store.ListMessagesByConversation(ctx, conversationID)
	if err != nil {
		return "", fmt.Errorf("conversation history: %w", err)
	}
	purpose, ok := r.workspacePurpose(ctx, workspaceID)
	if !ok {
		// Nothing can be shown to be the agent's own side: no note.
		return "", nil
	}
	// A user message stored at submit, before routing, has no origin of
	// its own: its turn's is on the reply logged with it.
	dispatchOrigin := map[string]string{}
	for _, m := range msgs {
		if m.DispatchID != "" && m.Origin != "" {
			dispatchOrigin[m.DispatchID] = m.Origin
		}
	}
	current := turnLogFrom(ctx).dispatchID
	kept := msgs[:0:0]
	for _, m := range msgs {
		if current != "" && m.DispatchID == current {
			continue
		}
		origin := m.Origin
		if origin == "" && m.DispatchID != "" {
			origin = dispatchOrigin[m.DispatchID]
		}
		if origin != registry.MessageOriginNone && origin != purpose {
			continue
		}
		kept = append(kept, m)
	}
	for i := len(kept) - 1; turnLogFrom(ctx).carriesOut && i >= 0; i-- {
		if kept[i].Role == registry.MessageRoleUser && kept[i].Content == message {
			kept = kept[:i]
			break
		}
	}
	turns := boundHistory(kept)
	if len(turns) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("[Loomux note: you are joining a conversation already under way. What was said so far, oldest first:\n")
	for _, t := range turns {
		who := "User"
		if t.Role == string(registry.MessageRoleAssistant) {
			who = "Loomux"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, t.Content)
	}
	b.WriteString("The user's latest message, the one for you, follows.]")
	return b.String(), nil
}

// workspacePurpose is the purpose of workspaceID's target. ok is false
// when the workspace or its target can't be read.
func (r *Router) workspacePurpose(ctx context.Context, workspaceID string) (purpose string, ok bool) {
	ws, err := r.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", false
	}
	return r.targetPurpose(ctx, ws.TargetID)
}

// targetPurpose is targetID's purpose, an empty one read as personal. ok
// is false when the target can't be read.
func (r *Router) targetPurpose(ctx context.Context, targetID string) (purpose string, ok bool) {
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return "", false
	}
	if target.Policy.Purpose == "" {
		return registry.TargetPurposePersonal, true
	}
	return target.Policy.Purpose, true
}

// withTurnOrigin records on ctx's turn log that the turn acts on
// targetID, for the Origin of the messages it logs.
func (r *Router) withTurnOrigin(ctx context.Context, targetID string) context.Context {
	tl := turnLogFrom(ctx)
	tl.originTarget = targetID
	tl.origin = "?" // the target can't be read: unknown, not "none"
	if purpose, ok := r.targetPurpose(ctx, targetID); ok {
		tl.origin = purpose
	}
	return withTurnLog(ctx, tl)
}

// turnOrigin is the Origin of a message the turn logs about taskID (empty
// for none): the purpose of the task's target; failing a task, the target
// the turn acts on; failing that, MessageOriginNone. A target that can't
// be read gives "", unknown.
func (r *Router) turnOrigin(ctx context.Context, taskID string) string {
	if taskID != "" {
		task, err := r.store.GetTask(ctx, taskID)
		if err != nil {
			return ""
		}
		purpose, _ := r.workspacePurpose(ctx, task.WorkspaceID)
		return purpose
	}
	switch origin := turnLogFrom(ctx).origin; origin {
	case "":
		return registry.MessageOriginNone
	case "?":
		return ""
	default:
		return origin
	}
}
