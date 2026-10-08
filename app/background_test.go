package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry"
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
