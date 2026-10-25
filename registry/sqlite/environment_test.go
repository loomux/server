package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// Two stores on one file stand in for two processes: creates racing
// for a plugin's last place, split across them, admit exactly one
// (LOOM-244).
func TestCreateEnvironmentWithinAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")
	var stores []*sqlite.Store
	for range 2 {
		s, err := sqlite.Open(path, sqlite.WithMasterKey(testMasterKey()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores = append(stores, s)
	}
	plugin := &registry.Plugin{ID: "p-race", Name: "fake", Label: "race", Version: "0.1.0", Protocol: "loomux-plugin/1",
		Source: registry.PluginSourceBundled, Path: "/fake", Trust: registry.PluginTrustBundled,
		Status: registry.PluginStatusInstalled, Enabled: true}
	if err := stores[0].CreatePlugin(ctx, plugin); err != nil {
		t.Fatal(err)
	}
	const limit, racers = 5, 16
	env := func(i int) *registry.Environment {
		target := &registry.Target{ID: fmt.Sprintf("t-race-%d", i), Name: fmt.Sprintf("race-%d", i), Kind: registry.TargetKindRemote,
			Host: fmt.Sprintf("r%d.fake.invalid", i), User: "agent"}
		if err := stores[0].CreateTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
		return &registry.Environment{ID: fmt.Sprintf("race%d", i), TargetID: target.ID, PluginID: plugin.ID, PluginName: "fake",
			Status: registry.EnvironmentCreating, Size: "small", HostKey: "ssh-ed25519 AAAA host", HostPrivateKey: []byte("key")}
	}
	for i := range limit - 1 {
		if err := stores[0].CreateEnvironmentWithin(ctx, env(i), limit); err != nil {
			t.Fatal(err)
		}
	}
	envs := make([]*registry.Environment, racers)
	for i := range envs {
		envs[i] = env(limit + i)
	}

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = stores[i%2].CreateEnvironmentWithin(ctx, envs[i], limit)
		}()
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, registry.ErrQuota):
			t.Errorf("racer %d: %v", i, err)
		}
	}
	list, err := stores[1].ListEnvironmentsByPlugin(ctx, plugin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if admitted != 1 || len(list) != limit {
		t.Errorf("%d racers admitted, %d environments; want 1 and %d", admitted, len(list), limit)
	}
}
