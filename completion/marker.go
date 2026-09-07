package completion

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// MarkerDir resolves the effective marker directory: configured
// unchanged if non-empty, else the same $TMPDIR/loomux/completion-markers
// default NewDetector has always fallen back to (same convention family
// as LOOM-4's $TMPDIR/loomux/ssh-cm for SSH ControlPath). Exported
// (LOOM-32) so a caller that needs the real effective directory — e.g.
// router.Router, to compute a per-task marker path for env-var
// injection — can resolve exactly what NewDetector itself will resolve,
// without constructing a Detector first.
func MarkerDir(configured string) string {
	if configured != "" {
		return configured
	}
	return filepath.Join(os.TempDir(), "loomux", "completion-markers")
}

// MarkerPath returns the deterministic marker file path for a task ID
// under dir — the same convention MarkerWatcher.MarkerPath applies via
// its own configured dir. A package-level function (LOOM-32) so a
// caller without a *MarkerWatcher instance (router.Router, building a
// launch command before any watcher is involved) can compute the exact
// same path a hook/notify script should be told to touch.
func MarkerPath(dir, taskID string) string {
	return filepath.Join(dir, taskID+".done")
}

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
	return MarkerPath(w.dir, taskID)
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
