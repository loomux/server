package completion

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// defaultMarkerDirScript creates the default marker directory on the
// target, as the target's own user, and prints its path. Default because
// LOOMUX_MARKER_DIR is unset (LOOM-75 review). It lives under $HOME rather
// than a fixed /tmp path: on a shared target another user could create
// /tmp/loomux/... first, so the hook's mkdir fails and turns hang, or
// write markers there that end turns early. The script refuses a
// symlink, or a directory it doesn't own, and tightens the mode to 0700.
// $HOME rather than $XDG_RUNTIME_DIR: the tmux server that runs the agent
// and the ssh session that runs this script can see different
// XDG_RUNTIME_DIR values, and both sides must agree on the path. Wrapped
// in sh -c because a target's login shell can be fish (sc1). No
// backslashes: fish's single quotes would read them as escapes.
const defaultMarkerDirScript = `d="${HOME:?}/.cache/loomux/completion-markers"
umask 077
mkdir -p "$d" 2>/dev/null
if [ -L "$d" ] || [ ! -d "$d" ] || [ ! -O "$d" ]; then
  echo "marker dir $d is not a directory owned by $(id -un)" >&2
  exit 1
fi
chmod 700 "$d" && echo "$d"
`

// ResolveMarkerDir returns the marker directory to use on the target
// behind exec. A configured directory (LOOMUX_MARKER_DIR) is returned
// verbatim, without touching the target. On a shared target, configure
// only a directory that belongs to the target user. With nothing
// configured, the per-user default is created and checked on the target
// (defaultMarkerDirScript). Both router (to tell the launched agent its
// LOOMUX_MARKER_PATH) and MarkerWatcher (to watch for it) resolve it this
// way, so they agree on the path.
func ResolveMarkerDir(ctx context.Context, exec interface {
	RunOnce(ctx context.Context, command string) (string, error)
}, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	out, err := exec.RunOnce(ctx, "sh -c "+shellQuote(defaultMarkerDirScript))
	if err != nil {
		return "", fmt.Errorf("completion: marker dir: %s: %w", strings.TrimSpace(out), err)
	}
	dir := strings.TrimSpace(out)
	if !strings.HasPrefix(dir, "/") || strings.Contains(dir, "\n") {
		return "", fmt.Errorf("completion: marker dir: unexpected output %q", out)
	}
	return dir, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// NewMarkerWatcher constructs a MarkerWatcher. dir is the configured
// marker directory; empty means the per-user default, resolved on each
// task's target by ResolveMarkerDir.
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
	dir, err := ResolveMarkerDir(ctx, exec, w.dir)
	if err != nil {
		return err
	}
	path := MarkerPath(dir, task.ID)

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
