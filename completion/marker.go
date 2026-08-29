package completion

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Loomux/server/registry"
)

// MarkerWatcher implements the shared mechanism behind design spec §5's
// tiers 1 and 2 (native hook vs. prompt-engineered self-report): watch a
// deterministic per-task marker file for existence. Content doesn't
// matter — existence is the signal — so both a native hook (a one-line
// `touch <path>`) and a prompt-engineered self-report (an agent with
// shell access told to touch the path when done) can satisfy it
// identically.
type MarkerWatcher struct {
	dir          string
	pollInterval time.Duration
}

// NewMarkerWatcher constructs a MarkerWatcher. dir is the base directory
// marker files live under.
func NewMarkerWatcher(dir string, pollInterval time.Duration) *MarkerWatcher {
	return &MarkerWatcher{dir: dir, pollInterval: pollInterval}
}

// MarkerPath returns the deterministic marker file path for a task ID.
func (w *MarkerWatcher) MarkerPath(taskID string) string {
	return filepath.Join(w.dir, taskID+".done")
}

// Wait blocks until task's marker file exists, or ctx is done. On
// success, the marker file is removed (best-effort) so a long-running
// server doesn't accumulate stale marker files.
func (w *MarkerWatcher) Wait(ctx context.Context, task *registry.Task) error {
	path := w.MarkerPath(task.ID)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(path); err == nil {
			_ = os.Remove(path)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
