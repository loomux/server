package app

import (
	"context"
	"log/slog"

	"github.com/Loomux/server/registry"
)

// unheldTakeovers returns the tasks in human-takeover with no active
// drive hold (LOOM-263). Since holds exist a task enters human-takeover
// only with one, so each of these was taken before the task_holds
// migration, or lost its hold some other way: nothing shows who has it,
// and nothing will hand it back.
func unheldTakeovers(ctx context.Context, store registry.Store) ([]*registry.Task, error) {
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	holds, err := store.ListActiveTaskHolds(ctx)
	if err != nil {
		return nil, err
	}
	driven := make(map[string]bool, len(holds))
	for _, h := range holds {
		if h.Mode == registry.HoldModeDrive {
			driven[h.TaskID] = true
		}
	}
	var out []*registry.Task
	for _, t := range tasks {
		if t.Status == registry.TaskStatusHumanTakeover && !driven[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

// warnUnheldTakeovers logs each task unheldTakeovers finds, at startup
// (docs/design/tui-client-design.md, "Checks before building"). It only
// reports: the task is left as it is, for a person to take or release.
func warnUnheldTakeovers(ctx context.Context, store registry.Store, logger *slog.Logger) {
	if logger == nil {
		return
	}
	tasks, err := unheldTakeovers(ctx, store)
	if err != nil {
		logger.Warn("could not check for tasks in human takeover without a hold", "err", err)
		return
	}
	for _, t := range tasks {
		logger.Warn("task is in human takeover with no hold: nothing says who has it; take it over or release it",
			"task_id", t.ID, "conversation_id", t.ConversationID, "workspace_id", t.WorkspaceID, "updated_at", t.UpdatedAt)
	}
}
