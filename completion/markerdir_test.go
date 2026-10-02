package completion_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// countingExecutor counts RunOnce calls on top of a real local executor.
type countingExecutor struct {
	*targets.LocalExecutor
	runOnce int
}

func (e *countingExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	e.runOnce++
	return e.LocalExecutor.RunOnce(ctx, command)
}

// The default marker dir (LOOMUX_MARKER_DIR unset) is per target user,
// created 0700 on the target itself: a shared /tmp/loomux path could be
// pre-created by another user of the target (sc1 is shared), making
// mkdir fail or letting them plant markers that end turns early.
func TestResolveMarkerDir_DefaultIsPerUser0700(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := completion.ResolveMarkerDir(context.Background(), targets.NewLocalExecutor(), "")
	if err != nil {
		t.Fatalf("ResolveMarkerDir: %v", err)
	}
	if want := filepath.Join(home, ".cache", "loomux", "completion-markers"); dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want a 0700 directory", fi.Mode())
	}
}

func TestResolveMarkerDir_TightensExistingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".cache", "loomux", "completion-markers")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := completion.ResolveMarkerDir(context.Background(), targets.NewLocalExecutor(), ""); err != nil {
		t.Fatalf("ResolveMarkerDir: %v", err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want tightened to 0700", fi.Mode().Perm())
	}
}

// A symlink in place of the marker dir could point anywhere, including a
// directory someone else controls — refuse it rather than follow it.
func TestResolveMarkerDir_RefusesSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := filepath.Join(home, ".cache", "loomux")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(parent, "completion-markers")); err != nil {
		t.Fatal(err)
	}
	_, err := completion.ResolveMarkerDir(context.Background(), targets.NewLocalExecutor(), "")
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("err = %v, want a refusal naming the ownership requirement", err)
	}
}

// LOOMUX_MARKER_DIR is used verbatim, without touching the target.
func TestResolveMarkerDir_ConfiguredIsVerbatim(t *testing.T) {
	exec := &countingExecutor{LocalExecutor: targets.NewLocalExecutor()}
	dir, err := completion.ResolveMarkerDir(context.Background(), exec, "/configured/dir")
	if err != nil || dir != "/configured/dir" {
		t.Fatalf("= %q, %v; want the configured dir", dir, err)
	}
	if exec.runOnce != 0 {
		t.Fatalf("RunOnce called %d times for a configured dir", exec.runOnce)
	}
}

// The watcher watches the same per-user default dir the launch was told.
func TestMarkerWatcher_DefaultDirResolvedOnTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	w := completion.NewMarkerWatcher(localExecutorFactory(), "", 20*time.Millisecond)
	task := &registry.Task{ID: "task-default-dir"}

	done := make(chan error, 1)
	go func() { done <- w.Wait(context.Background(), task, localTarget) }()

	marker := filepath.Join(home, ".cache", "loomux", "completion-markers", task.ID+".done")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Dir(marker)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never created the default marker dir")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not see the marker in the default dir")
	}
}
