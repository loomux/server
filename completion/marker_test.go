package completion_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

func localExecutorFactory() func(*registry.Target) (targets.TargetExecutor, error) {
	return func(*registry.Target) (targets.TargetExecutor, error) {
		return targets.NewLocalExecutor(), nil
	}
}

var localTarget = &registry.Target{Kind: registry.TargetKindLocal}

func TestMarkerWatcher_WaitReturnsWhenMarkerAppears(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(localExecutorFactory(), dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-1"}

	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(context.Background(), task, localTarget) }()

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
	w := completion.NewMarkerWatcher(localExecutorFactory(), dir, 20*time.Millisecond)

	got := w.MarkerPath("abc-123")
	want := filepath.Join(dir, "abc-123.done")
	if got != want {
		t.Fatalf("MarkerPath = %q, want %q", got, want)
	}
}

func TestMarkerWatcher_RemovesMarkerAfterDetection(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(localExecutorFactory(), dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-cleanup"}

	if err := os.WriteFile(w.MarkerPath(task.ID), []byte("done"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := w.Wait(context.Background(), task, localTarget); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if _, err := os.Stat(w.MarkerPath(task.ID)); !os.IsNotExist(err) {
		t.Fatalf("marker file still exists after detection: err = %v", err)
	}
}

func TestMarkerWatcher_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(localExecutorFactory(), dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-never"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := w.Wait(ctx, task, localTarget)
	if err == nil {
		t.Fatalf("Wait: got nil error, want a context-deadline error since no marker was ever written")
	}
}

// remoteExecutorFactory points a RemoteExecutor at an sshtest loopback
// server — the same trick targets/remote_test.go already uses: the real
// ssh client talks to an in-process fake server whose "exec" handler
// actually runs commands locally, so a test can write the marker to a
// real local path and see the "remote" FileExists/RemoveFile calls
// observe it for real.
func remoteExecutorFactory(server *sshtest.Server) func(*registry.Target) (targets.TargetExecutor, error) {
	return func(*registry.Target) (targets.TargetExecutor, error) {
		return targets.NewRemoteExecutor(server.Host, "loomux-test",
			targets.WithPort(server.Port),
			targets.WithIdentityFile(server.IdentityFile),
			targets.WithExtraSSHArgs(
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
			),
		), nil
	}
}

var remoteTarget = &registry.Target{Kind: registry.TargetKindRemote}

func TestMarkerWatcher_RemoteViaSSH(t *testing.T) {
	server := sshtest.Start(t)
	dir := t.TempDir()
	w := completion.NewMarkerWatcher(remoteExecutorFactory(server), dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-remote"}

	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(context.Background(), task, remoteTarget) }()

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
		t.Fatalf("Wait did not return after the marker appeared (remote)")
	}

	if _, err := os.Stat(w.MarkerPath(task.ID)); !os.IsNotExist(err) {
		t.Fatalf("marker file still exists after remote detection: err = %v", err)
	}
}

func TestMarkerWatcher_RemoteUnreachable(t *testing.T) {
	dir := t.TempDir()
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		return targets.NewRemoteExecutor("127.0.0.1", "loomux-test",
			targets.WithPort(unusedPort(t)),
			targets.WithConnectTimeout("2s"),
			targets.WithExtraSSHArgs(
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
			),
		), nil
	}
	w := completion.NewMarkerWatcher(factory, dir, 20*time.Millisecond)
	task := &registry.Task{ID: "task-unreachable"}

	err := w.Wait(context.Background(), task, remoteTarget)
	if !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("Wait against an unreachable target: err = %v, want ErrUnreachable", err)
	}
}

// unusedPort finds a port nothing is listening on by briefly binding and
// releasing it — good enough for a test that needs a connection attempt
// to fail quickly, not a race-free reservation. Mirrors
// targets/remote_test.go's own helper of the same name.
func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unusedPort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
