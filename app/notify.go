package app

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry"
)

// notifyTimeout bounds one notification's delivery.
const notifyTimeout = 15 * time.Second

// withheldSummary is a notification's body when the vault can't be read
// to scrub the real one: it leaves Loomux, so it fails closed.
const withheldSummary = "Open Loomux to see it."

// turnNotifier tells the user when a turn they may not be watching ends
// (LOOM-102): done, failed, or stopped on a prompt only they can answer.
// A turn shorter than minDuration is skipped — they were most likely
// still looking at it.
type turnNotifier struct {
	store       registry.Store
	filter      *notify.Filter
	minDuration time.Duration
	// publicURL is where the web client is served, for the link; empty
	// means no link.
	publicURL string
	logger    *slog.Logger
}

// finished is the dispatch service's WithOnFinished hook: it builds and
// sends the notification off the job's goroutine.
func (n *turnNotifier) finished(d *registry.Dispatch) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		e, ok := n.event(ctx, d)
		if !ok {
			return
		}
		n.send(ctx, e, n.logger.With("dispatch_id", d.ID, "kind", string(e.Kind)))
	}()
}

// lateReply is the router's WithLateReplyHook (LOOM-121): an agent
// reported after the turn it ended early. There is no turn length to go
// by, and the user has likely moved on: it is always news.
func (n *turnNotifier) lateReply(task *registry.Task, reply string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		e := n.lateReplyEvent(ctx, task, reply)
		n.send(ctx, e, n.logger.With("task_id", task.ID, "kind", string(e.Kind)))
	}()
}

// lateReplyEvent builds the notification for task's late reply.
func (n *turnNotifier) lateReplyEvent(ctx context.Context, task *registry.Task, reply string) notify.Event {
	e := notify.Event{Kind: notify.KindDone, Summary: strings.TrimSpace(reply)}
	if ws, err := n.store.GetWorkspace(ctx, task.WorkspaceID); err == nil {
		e.Workspace = ws.Name
	}
	if n.taskRelay(ctx, task) == registry.RelayNone {
		e.Summary = withheldSummary
	}
	return n.finish(ctx, e, task.ConversationID)
}

func (n *turnNotifier) send(ctx context.Context, e notify.Event, log *slog.Logger) {
	sent, err := n.filter.Notify(ctx, e)
	switch {
	case err != nil:
		log.Warn("notification not delivered", "error", err)
	case sent:
		log.Info("notification sent")
	}
}

// event builds d's notification, or reports false when d doesn't warrant
// one.
func (n *turnNotifier) event(ctx context.Context, d *registry.Dispatch) (notify.Event, bool) {
	// A turn the user cancelled is no news to them.
	if d.FinishedAt == nil || d.ErrorClass == registry.ErrorClassCancelled {
		return notify.Event{}, false
	}
	start := d.CreatedAt
	if d.StartedAt != nil {
		start = *d.StartedAt
	}
	if d.FinishedAt.Sub(start) < n.minDuration {
		return notify.Event{}, false
	}

	e := notify.Event{Kind: notify.KindDone, Summary: strings.TrimSpace(d.Reply)}
	task := n.turnTask(ctx, d)
	if task != nil {
		if ws, err := n.store.GetWorkspace(ctx, task.WorkspaceID); err == nil {
			e.Workspace = ws.Name
		}
		if task.Status == registry.TaskStatusNeedsAttention {
			e.Kind = notify.KindNeedsYou
			if s := attentionSummary(task.Attention); s != "" {
				e.Summary = s
			}
		}
	}
	// A turn that ended on an offer waits on the user's Approve or Deny
	// (LOOM-123), which the link opens.
	if d.Status == registry.DispatchStatusSucceeded && n.madeOffer(ctx, d) {
		e.Kind = notify.KindNeedsYou
	}
	if d.Status == registry.DispatchStatusFailed {
		e.Kind, e.Summary = notify.KindFailed, d.Error
	}
	// A notification leaves Loomux like a router model's input: under a
	// target's relay policy none, the body says nothing of its work —
	// not the agent's reply, its prompt, nor an error that can quote it.
	if n.turnRelay(ctx, d, task) == registry.RelayNone {
		e.Summary = withheldSummary
	}
	return n.finish(ctx, e, d.ConversationID), true
}

// turnRelay is the relay policy of the target d's turn acted on: its
// task's target, or for a turn without one (an offer, a failure before
// launch), the target its logged messages record. A turn that touched no
// target is full.
func (n *turnNotifier) turnRelay(ctx context.Context, d *registry.Dispatch, task *registry.Task) string {
	if task != nil {
		return n.taskRelay(ctx, task)
	}
	msgs, err := n.store.ListMessagesByConversation(ctx, d.ConversationID)
	if err != nil {
		return registry.RelayNone
	}
	for _, m := range msgs {
		if m.DispatchID == d.ID && m.OriginTargetID != "" {
			return n.targetRelay(ctx, m.OriginTargetID)
		}
	}
	return registry.RelayFull
}

// taskRelay is the relay policy of task's target; one that can't be read
// says nothing (none).
func (n *turnNotifier) taskRelay(ctx context.Context, task *registry.Task) string {
	ws, err := n.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return registry.RelayNone
	}
	return n.targetRelay(ctx, ws.TargetID)
}

func (n *turnNotifier) targetRelay(ctx context.Context, targetID string) string {
	t, err := n.store.GetTarget(ctx, targetID)
	if err != nil {
		return registry.RelayNone
	}
	return t.Policy.EffectiveRelay()
}

// madeOffer reports whether d's turn left an offer awaiting an answer.
func (n *turnNotifier) madeOffer(ctx context.Context, d *registry.Dispatch) bool {
	confs, err := n.store.ListConfirmationsByConversation(ctx, d.ConversationID)
	if err != nil {
		return false
	}
	for _, c := range confs {
		if c.DispatchID == d.ID && c.Status == registry.ConfirmationPending {
			return true
		}
	}
	return false
}

// finish scrubs e's body and links it to conversationID.
func (n *turnNotifier) finish(ctx context.Context, e notify.Event, conversationID string) notify.Event {
	// The body leaves Loomux, often for a shared ntfy server: every
	// credential value in the vault is scrubbed from it first.
	summary, err := credentials.RedactAll(ctx, n.store, e.Summary)
	if err != nil {
		summary = withheldSummary
	}
	e.Summary = summary
	if n.publicURL != "" {
		e.Link = strings.TrimRight(n.publicURL, "/") + "/conversations/" + url.PathEscape(conversationID)
	}
	return e
}

// turnTask is the task d's turn worked in: the conversation's most
// recently updated task, if it was updated during the turn. None (nil)
// when the router answered directly.
func (n *turnNotifier) turnTask(ctx context.Context, d *registry.Dispatch) *registry.Task {
	tasks, err := n.store.ListTasks(ctx)
	if err != nil {
		return nil
	}
	var latest *registry.Task
	for _, t := range tasks {
		if t.ConversationID != d.ConversationID || t.UpdatedAt.Before(d.CreatedAt) {
			continue
		}
		if latest == nil || t.UpdatedAt.After(latest.UpdatedAt) {
			latest = t
		}
	}
	return latest
}

// attentionSummary is one line saying what a prompt asks:
// "Bash command: kubectl delete pod x — Do you want to proceed?".
func attentionSummary(a *registry.Attention) string {
	if a == nil {
		return ""
	}
	head := a.Title
	if a.Detail != "" {
		if head != "" {
			head += ": "
		}
		head += a.Detail
	}
	switch {
	case head == "":
		return a.Question
	case a.Question == "":
		return head
	}
	return head + " — " + a.Question
}
