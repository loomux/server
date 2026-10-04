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
		sent, err := n.filter.Notify(ctx, e)
		log := n.logger.With("dispatch_id", d.ID, "kind", string(e.Kind))
		switch {
		case err != nil:
			log.Warn("notification not delivered", "error", err)
		case sent:
			log.Info("notification sent")
		}
	}()
}

// event builds d's notification, or reports false when d doesn't warrant
// one.
func (n *turnNotifier) event(ctx context.Context, d *registry.Dispatch) (notify.Event, bool) {
	if d.FinishedAt == nil {
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
	if task := n.turnTask(ctx, d); task != nil {
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
	if d.Status == registry.DispatchStatusFailed {
		e.Kind, e.Summary = notify.KindFailed, d.Error
	}
	// The body leaves Loomux, often for a shared ntfy server: every
	// credential value in the vault is scrubbed from it first.
	summary, err := credentials.RedactAll(ctx, n.store, e.Summary)
	if err != nil {
		summary = withheldSummary
	}
	e.Summary = summary
	if n.publicURL != "" {
		e.Link = strings.TrimRight(n.publicURL, "/") + "/conversations/" + url.PathEscape(d.ConversationID)
	}
	return e, true
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
