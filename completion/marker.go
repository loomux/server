package completion

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// MarkerWatcher implements the shared mechanism behind design spec §5's
// tiers 1 and 2 (native hook vs. prompt-engineered self-report): watch a
// deterministic per-task marker file for existence. Content doesn't
// matter — existence is the signal — so both a native hook (a one-line
// `touch <path>`) and a prompt-engineered self-report (an agent with
// shell access told to touch the path when done) can satisfy it
// identically. Goes through TargetExecutor.FileExists/RemoveFile for
// both local and remote targets — one code path, same spirit as tiers
// 1+2 sharing one mechanism in the first place, rather than a raw
// os.Stat for local and a separate mechanism for remote (LOOM-11).
type MarkerWatcher struct {
	newExecutor  orchestrator.ExecutorFactory
	dir          string
	pollInterval time.Duration
}

// NewMarkerWatcher constructs a MarkerWatcher. dir is the base directory
// marker files live under.
func NewMarkerWatcher(newExecutor orchestrator.ExecutorFactory, dir string, pollInterval time.Duration) *MarkerWatcher {
	return &MarkerWatcher{newExecutor: newExecutor, dir: dir, pollInterval: pollInterval}
}

// MarkerPath returns the deterministic marker file path for a task ID.
func (w *MarkerWatcher) MarkerPath(taskID string) string {
	return filepath.Join(w.dir, taskID+".done")
}

// Wait blocks until task's marker file exists on target's filesystem, or
// ctx is done. On success, the marker file is removed (best-effort) so
// a long-running server doesn't accumulate stale marker files. Unlike
// the interface it partially resembles, Wait takes target explicitly —
// it needs it to build the right executor (local vs. remote), the same
// reason IdleWatcher.Wait already takes one.
func (w *MarkerWatcher) Wait(ctx context.Context, task *registry.Task, target *registry.Target) error {
	exec, err := w.newExecutor(target)
	if err != nil {
		return err
	}
	path := w.MarkerPath(task.ID)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		exists, err := exec.FileExists(ctx, path)
		if err != nil {
			return err
		}
		if exists {
			_ = exec.RemoveFile(ctx, path)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
