package providers_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// blockingStore's GetEnvironment holds until its context ends: a notify
// in flight when the app closes.
type blockingStore struct {
	registry.Store
	entered  chan struct{}
	returned atomic.Bool
	calls    atomic.Int32
}

func (s *blockingStore) GetEnvironment(ctx context.Context, id string) (*registry.Environment, error) {
	s.calls.Add(1)
	s.entered <- struct{}{}
	<-ctx.Done()
	s.returned.Store(true)
	return nil, ctx.Err()
}

// LOOM-196: a targets.changed notify reconciles on a goroutine of its own.
// Close cancels one in flight and waits for it, so nothing uses the store
// after the app closes it; and a notify once closing starts nothing.
func TestClose_WaitsForInFlightNotify(t *testing.T) {
	store := &blockingStore{entered: make(chan struct{}, 1)}
	m := providers.NewManager(store, nil, "inst", nil, nil)
	params, _ := json.Marshal(protocol.TargetsChanged{ID: "env-1"})

	m.HandleNotify("plugin-1", protocol.NotifyTargetsChanged, params)
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the notify never read the store")
	}
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		if !store.returned.Load() {
			t.Fatal("Close returned while the notify was still using the store")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't cancel the notify in flight (it holds for a minute otherwise)")
	}

	m.HandleNotify("plugin-1", protocol.NotifyTargetsChanged, params)
	time.Sleep(50 * time.Millisecond)
	if n := store.calls.Load(); n != 1 {
		t.Fatalf("store read %d times; a notify after Close must not run", n)
	}
}

// countingStore counts GetEnvironment, the first step of a notify's
// reconcile, and answers with another plugin's machine so it stops
// there; hold, when set, keeps each call until it is closed.
type countingStore struct {
	registry.Store
	calls atomic.Int32
	hold  chan struct{}
}

func (s *countingStore) GetEnvironment(ctx context.Context, id string) (*registry.Environment, error) {
	s.calls.Add(1)
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
		}
	}
	return &registry.Environment{ID: id, PluginID: "someone-else"}, nil
}

func notify(m *providers.Manager, pluginID, id string) {
	params, _ := json.Marshal(protocol.TargetsChanged{ID: id, Status: protocol.EnvError})
	m.HandleNotify(pluginID, protocol.NotifyTargetsChanged, params)
}

// eventually waits up to 5 s for f.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// LOOM-213: a burst of targets.changed for one machine is one
// reconcile; another machine's is its own.
func TestNotifyCoalescesABurst(t *testing.T) {
	store := &countingStore{}
	m := providers.NewManager(store, nil, "inst", nil, nil)
	m.NotifyDelay = 50 * time.Millisecond
	defer m.Close()
	for i := 0; i < 20; i++ {
		notify(m, "plugin-1", "env-1")
	}
	notify(m, "plugin-1", "env-2")
	eventually(t, "the burst was never reconciled", func() bool { return store.calls.Load() >= 2 })
	time.Sleep(200 * time.Millisecond)
	if n := store.calls.Load(); n != 2 {
		t.Errorf("reconciles = %d for a burst of 20 on env-1 and one on env-2, want 2", n)
	}
}

// Hints that come while the machine's reconcile runs are one more
// reconcile after it: a state reached mid-reconcile isn't missed.
func TestNotifyDuringReconcileRunsOnceMore(t *testing.T) {
	store := &countingStore{hold: make(chan struct{})}
	m := providers.NewManager(store, nil, "inst", nil, nil)
	m.NotifyDelay = 10 * time.Millisecond
	defer m.Close()
	notify(m, "plugin-1", "env-1")
	eventually(t, "the reconcile never started", func() bool { return store.calls.Load() == 1 })
	for i := 0; i < 5; i++ {
		notify(m, "plugin-1", "env-1")
	}
	close(store.hold)
	eventually(t, "no reconcile after the hints that came meanwhile", func() bool { return store.calls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := store.calls.Load(); n != 2 {
		t.Errorf("reconciles = %d, want 2: the first, and one for the five during it", n)
	}
	notify(m, "plugin-1", "env-1") // all done: a new hint is a new reconcile
	eventually(t, "a later hint wasn't reconciled", func() bool { return store.calls.Load() == 3 })
}

// Any other notification, or a targets.changed without an id, is
// ignored: notifications are hints, and a newer plugin may send ones
// this host doesn't know.
func TestNotifyIgnoresWhatItDoesntKnow(t *testing.T) {
	store := &countingStore{}
	m := providers.NewManager(store, nil, "inst", nil, nil)
	m.NotifyDelay = time.Millisecond
	defer m.Close()
	m.HandleNotify("plugin-1", "targets.from_the_future", json.RawMessage(`{"id":"env-1"}`))
	m.HandleNotify("plugin-1", protocol.NotifyTargetsChanged, json.RawMessage(`{"status":"error"}`))
	m.HandleNotify("plugin-1", protocol.NotifyTargetsChanged, json.RawMessage(`not json`))
	time.Sleep(100 * time.Millisecond)
	if n := store.calls.Load(); n != 0 {
		t.Errorf("reconciles = %d, want 0", n)
	}
}
