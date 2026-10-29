package router

import (
	"context"
	"fmt"
	"strings"
)

// affirmativeWords are the words a bare affirmative is made of; at least
// one of affirmativeCore must be among them ("it" or "please" alone
// agrees to nothing).
var (
	affirmativeWords = map[string]bool{
		"y": true, "yes": true, "yep": true, "yeah": true, "yup": true, "ya": true,
		"ok": true, "okay": true, "k": true, "sure": true, "alright": true, "fine": true,
		"confirm": true, "confirmed": true, "proceed": true, "absolutely": true, "definitely": true,
		"go": true, "ahead": true, "do": true, "run": true, "start": true, "for": true, "it": true,
		"please": true, "sounds": true, "good": true, "lets": true, "let's": true, "👍": true,
	}
	affirmativeCore = map[string]bool{
		"y": true, "yes": true, "yep": true, "yeah": true, "yup": true, "ya": true,
		"ok": true, "okay": true, "k": true, "sure": true, "alright": true, "fine": true,
		"confirm": true, "confirmed": true, "proceed": true, "absolutely": true, "definitely": true,
		"go": true, "do": true, "run": true, "start": true, "good": true, "👍": true,
	}
)

// isBareAffirmative reports whether message only agrees ("yes", "ok",
// "go", "do it", "sure, go ahead") and asks for nothing of its own.
func isBareAffirmative(message string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	m = strings.NewReplacer(",", " ", ".", " ", "!", " ", "’", "'").Replace(m)
	words := strings.Fields(m)
	core := false
	for _, w := range words {
		if !affirmativeWords[w] {
			return false
		}
		core = core || affirmativeCore[w]
	}
	return core
}

// noOfferReply answers a bare affirmative that has nothing to agree to.
const noOfferReply = "There's nothing waiting for a yes in this conversation, so I haven't started anything. " +
	"Tell me what you'd like done, and where."

// guardBareAffirmative keeps a bare affirmative from starting anything
// that this conversation didn't ask for. A recorded offer is answered
// before routing, so one reaching here agrees to nothing the router
// knows of: whatever the routing model read into it (a question in its
// own earlier answer, say), it is asked about instead of acted on, unless
// it continues work this conversation already has open: into the open
// task's workspace, or another workspace this conversation has used.
// asked is set when d was replaced.
func (r *Router) guardBareAffirmative(ctx context.Context, conversationID, message string, d Decision,
	open *OpenTaskSnapshot) (out Decision, asked bool, err error) {
	if d.Action == ActionAnswerDirectly || !isBareAffirmative(message) {
		return d, false, nil
	}
	reply := noOfferReply
	if open != nil {
		if d.Action != ActionUseWorkspace {
			reply = "I haven't started anything new: tell me what you'd like done, and where."
		} else {
			used, err := r.conversationUsed(ctx, conversationID, d.WorkspaceID)
			if err != nil {
				return d, false, err
			}
			if used {
				return d, false, nil
			}
			name := d.WorkspaceID
			if ws, err := r.store.GetWorkspace(ctx, d.WorkspaceID); err == nil {
				name = ws.Name
			}
			reply = fmt.Sprintf("That would start work in workspace %s, which this conversation hasn't used, "+
				"so I haven't started anything. Tell me what you'd like done there, or where.", name)
		}
	}
	return Decision{Action: ActionAnswerDirectly, DirectAnswer: reply, LeaveOpenTask: true,
		Model: d.Model, Tier: d.Tier}, true, nil
}

// conversationUsed reports whether conversationID has had a task in
// workspaceID.
func (r *Router) conversationUsed(ctx context.Context, conversationID, workspaceID string) (bool, error) {
	tasks, err := r.store.ListTasksByConversation(ctx, conversationID)
	if err != nil {
		return false, fmt.Errorf("conversation tasks: %w", err)
	}
	for _, t := range tasks {
		if t.WorkspaceID == workspaceID {
			return true, nil
		}
	}
	return false, nil
}
