package completion_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
)

func TestMarkerWatcher_WaitReturnsWhenMarkerAppears(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-1"}

	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(context.Background(), task) }()

	// Give Wait a moment to start polling before the marker exists, so
	// this also exercises the "not there yet" path, not just an
	// immediate check.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(w.MarkerPath(task.ID), []byte("done"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after the marker was written")
	}
}

func TestMarkerWatcher_MarkerPathIsDeterministicPerTask(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(dir, 20*time.Millisecond)

	got := w.MarkerPath("abc-123")
	want := filepath.Join(dir, "abc-123.done")
	if got != want {
		t.Fatalf("MarkerPath = %q, want %q", got, want)
	}
}

func TestMarkerWatcher_RemovesMarkerAfterDetection(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-cleanup"}

	if err := os.WriteFile(w.MarkerPath(task.ID), []byte("done"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := w.Wait(context.Background(), task); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if _, err := os.Stat(w.MarkerPath(task.ID)); !os.IsNotExist(err) {
		t.Fatalf("marker file still exists after detection: err = %v", err)
	}
}

func TestMarkerWatcher_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-never"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := w.Wait(ctx, task)
	if err == nil {
		t.Fatalf("Wait: got nil error, want a context-deadline error since no marker was ever written")
	}
}
