package app

import (
	"context"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// LOOM-163: Close waits for the store's background users before it
// closes the store, and starts none after.
func TestBuild_CloseWaitsForBackgroundWork(t *testing.T) {
	srv := fakeRouterServer(t)
	a, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	if !a.bg.Go(func() {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, err := a.store.ListTasks(context.Background())
		result <- err
	}) {
		t.Fatal("background refused work before Close")
	}
	<-started
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("background work read the store as it closed: %v", err)
		}
	default:
		t.Fatal("Close returned before the background work finished")
	}
	if a.bg.Go(func() {}) {
		t.Error("background accepted work after Close")
	}
}

// LOOM-195: Close waits for startup task reconciliation even while it
// is reaching a target. It used to run on a context of its own that
// Close neither cancelled nor waited for, so it went on using the store
// after Close: TestDrainDispatches_StopsSubmits failed in CI when its
// connection, opened after Close, recreated test.db in the data dir the
// test was removing ("directory not empty").
func TestBuild_CloseWaitsForStartupReconciliation(t *testing.T) {
	// A target that accepts SSH connections and never answers holds
	// reconciliation in its has-session check.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	ctx := context.Background()
	seed, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("sqlite.Open (seed): %v", err)
	}
	if err := seed.CreateTarget(ctx, &registry.Target{ID: "silent", Name: "silent", Kind: registry.TargetKindRemote,
		Host: "127.0.0.1", SSHPort: ln.Addr().(*net.TCPAddr).Port, User: "loomux-test"}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := seed.CreateWorkspace(ctx, &registry.Workspace{ID: "ws", Name: "ws", Path: "/srv/ws", TargetID: "silent",
		Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := seed.CreateTask(ctx, &registry.Task{ID: "left-running", WorkspaceID: "ws", Kind: registry.TaskKindCommand,
		TmuxSession: "loomux-left-running", Status: registry.TaskStatusRunning, ConversationID: "c"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close (seed): %v", err)
	}

	a, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); goroutinesIn("ReconcileTasks", "HasSession") == ""; {
		if time.Now().After(deadline) {
			_ = a.Close()
			t.Fatal("startup reconciliation never reached the target")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left := goroutinesIn("ReconcileTasks"); left != "" {
		t.Fatalf("startup reconciliation still running after Close:\n%s", left)
	}
}

// goroutinesIn returns the stacks of the goroutines whose stack names
// every one of funcs.
func goroutinesIn(funcs ...string) string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var match []string
next:
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, f := range funcs {
			if !strings.Contains(g, f) {
				continue next
			}
		}
		match = append(match, g)
	}
	return strings.Join(match, "\n\n")
}

// blockingNotifier delivers once release is closed.
type blockingNotifier struct {
	release   chan struct{}
	delivered atomic.Int32
}

func (b *blockingNotifier) Notify(ctx context.Context, e notify.Event) error {
	<-b.release
	b.delivered.Add(1)
	return nil
}

// LOOM-163: a notification on its way when the app closes is delivered
// before Close returns, not lost; one after is dropped, not run against
// a closed store.
func TestTurnNotifier_CloseWaitsForDelivery(t *testing.T) {
	store := openNotifyStore(t)
	sink := &blockingNotifier{release: make(chan struct{})}
	bg := &background{}
	n := &turnNotifier{
		store:  store,
		filter: notify.NewFilter(sink, notify.FilterConfig{Events: map[notify.Kind]bool{notify.KindDone: true}, Burst: 10, Refill: time.Second}),
		logger: slog.New(slog.DiscardHandler),
		bg:     bg,
	}
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Minute)
	d := &registry.Dispatch{ID: "d1", ConversationID: "c", Status: registry.DispatchStatusSucceeded, Reply: "done", CreatedAt: start, FinishedAt: &end}

	n.finished(d)
	closed := make(chan struct{})
	go func() {
		bg.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned with a notification still on its way")
	case <-time.After(100 * time.Millisecond):
	}
	close(sink.release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close never returned")
	}
	if got := sink.delivered.Load(); got != 1 {
		t.Fatalf("delivered %d notifications before close returned, want 1", got)
	}

	n.finished(d)
	time.Sleep(50 * time.Millisecond)
	if got := sink.delivered.Load(); got != 1 {
		t.Errorf("a notification after close was delivered (%d total)", got)
	}
}
