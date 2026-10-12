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
