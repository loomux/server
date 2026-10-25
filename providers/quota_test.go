package providers_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// countBarrier holds the next n machine counts until all n have read,
// so creates in different processes all count before any inserts: the
// interleaving that let two of them take the last place (LOOM-244).
type countBarrier struct {
	mu    sync.Mutex
	n     int
	ready chan struct{}
}

func (b *countBarrier) arm(n int) {
	b.mu.Lock()
	b.n, b.ready = n, make(chan struct{})
	b.mu.Unlock()
}

func (b *countBarrier) wait() {
	b.mu.Lock()
	if b.n == 0 {
		b.mu.Unlock()
		return
	}
	b.n--
	ready := b.ready
	if b.n == 0 {
		close(ready)
	}
	b.mu.Unlock()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
	}
}

// barrierStore is a store whose machine counts go through a barrier.
type barrierStore struct {
	registry.Store
	b *countBarrier
}

func (s barrierStore) ListEnvironmentsByPlugin(ctx context.Context, pluginID string) ([]*registry.Environment, error) {
	envs, err := s.Store.ListEnvironmentsByPlugin(ctx, pluginID)
	s.b.wait()
	return envs, err
}

// Two server processes on one database, each with its own plugin
// process, create the last machine the limit allows at the same moment:
// exactly one gets it, and the other is refused with ErrQuota and leaves
// no target or key behind (LOOM-244).
func TestCreateQuotaAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "p.db")
	b := &countBarrier{}
	wrap := func(s registry.Store) registry.Store { return barrierStore{Store: s, b: b} }
	one := newHarnessAt(t, nil, db, "", wrap)
	two := newHarnessAt(t, nil, db, one.pluginID, wrap)
	for i := 0; i < 4; i++ { // the fake's max_environments is 5
		one.create(t, "m"+string(rune('a'+i)), true)
	}

	b.arm(2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, h := range []*harness{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = h.m.Create(ctx, providers.CreateRequest{Target: registry.Target{Name: "last-" + string(rune('a'+i))}, PluginID: h.pluginID})
		}()
	}
	wg.Wait()

	won, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, providers.ErrQuota):
			refused++
		default:
			t.Errorf("Create: %v", err)
		}
	}
	if won != 1 || refused != 1 {
		t.Errorf("two creates for the last place: %d made, %d refused (%v); want 1 and 1", won, refused, errs)
	}
	envs, _ := one.store.ListEnvironmentsByPlugin(ctx, one.pluginID)
	targets, _ := one.store.ListTargets(ctx)
	keys, _ := one.store.ListSSHKeys(ctx)
	if len(envs) != 5 || len(targets) != 5 || len(keys) != 5 {
		t.Errorf("after the race: %d environments, %d targets, %d keys; want 5 of each", len(envs), len(targets), len(keys))
	}
}
