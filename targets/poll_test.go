package targets_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

// TestRemoteExecutor_PollAnswersInOneOperation: one Poll reports every
// file and pane asked about (LOOM-254) — an existing file (with a quote
// and a space in its name), a missing one, a running pane, a dead one
// and a missing session — in a single ssh operation.
func TestRemoteExecutor_PollAnswersInOneOperation(t *testing.T) {
	server := sshtest.Start(t)
	var ops atomic.Int64
	server.Exec = func(string, io.Reader, io.Writer, io.Writer) (int, bool) {
		ops.Add(1)
		return 0, false
	}
	exec := newTestRemoteExecutor(t, server)
	ctx := context.Background()

	dir := t.TempDir()
	present := filepath.Join(dir, "it's here.done")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.NewSession(ctx, "loomux-poll-running", dir, "sleep 30"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := exec.NewSession(ctx, "loomux-poll-dead", dir, "exit 3"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		exit, err := exec.PaneExited(ctx, "loomux-poll-dead")
		if err != nil {
			t.Fatalf("PaneExited: %v", err)
		}
		if exit != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead pane's process never exited")
		}
		time.Sleep(50 * time.Millisecond)
	}

	before := ops.Load()
	res, err := exec.Poll(ctx, targets.PollRequest{
		Files: []string{present, filepath.Join(dir, "missing.done")},
		Panes: []string{"loomux-poll-running", "loomux-poll-dead", "loomux-poll-missing"},
	})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if n := ops.Load() - before; n != 1 {
		t.Errorf("Poll took %d ssh operations, want 1", n)
	}
	if want := []bool{true, false}; !equalBools(res.Exists, want) {
		t.Errorf("Exists = %v, want %v", res.Exists, want)
	}
	if want := []bool{true, false, false}; !equalBools(res.Running, want) {
		t.Errorf("Running = %v, want %v", res.Running, want)
	}
}

// With no tmux server on the socket at all, every pane is "not running"
// (left to PaneExited), not an error.
func TestRemoteExecutor_PollWithoutTmuxServer(t *testing.T) {
	exec := newTestRemoteExecutor(t, sshtest.Start(t))
	res, err := exec.Poll(context.Background(), targets.PollRequest{Panes: []string{"loomux-none"}})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !equalBools(res.Running, []bool{false}) {
		t.Errorf("Running = %v, want [false]", res.Running)
	}
}

// The executors production builds for a remote target batch their polls;
// a wrapper that hid Poll would quietly bring back two ssh operations per
// waiting turn a second.
func TestNewExecutorWith_RemoteIsPoller(t *testing.T) {
	target := &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01", User: "loomux"}
	exec, err := targets.NewExecutorWith(target, targets.ExecutorOptions{Metrics: metrics.NewDiscard(), KnownHostsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewExecutorWith: %v", err)
	}
	if _, ok := exec.(targets.Poller); !ok {
		t.Errorf("remote executor %T isn't a targets.Poller", exec)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
