package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-197: two app instances in one process keep their pinned host keys
// apart, each in its own data dir. The known_hosts dir used to be
// process-wide and repointed by every Build, so the first instance's
// pins went to the second's dir.
func TestBuild_KnownHostsPerInstance(t *testing.T) {
	srv := fakeRouterServer(t)
	type instance struct {
		app *App
		dir string
	}
	var apps []instance
	for range 2 {
		cfg := testConfig(t, srv.URL)
		a, err := Build(cfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		apps = append(apps, instance{a, filepath.Dir(cfg.DBPath)})
	}
	ctx := context.Background()
	for i, inst := range apps {
		tgt := &registry.Target{ID: []string{"pinned-a", "pinned-b"}[i], Name: "pinned", Kind: registry.TargetKindRemote,
			Host: "pinned.invalid", User: "u"}
		if err := inst.app.Store().CreateTarget(ctx, tgt); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
		tgt.HostKeys = "pinned.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
		if _, err := inst.app.newExecutor(tgt); err != nil {
			t.Fatalf("newExecutor: %v", err)
		}
	}
	for i, inst := range apps {
		own, other := []string{"pinned-a", "pinned-b"}[i], []string{"pinned-b", "pinned-a"}[i]
		if _, err := os.Stat(filepath.Join(inst.dir, "known_hosts.d", own)); err != nil {
			t.Errorf("instance %d: its pin isn't in its own data dir: %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(inst.dir, "known_hosts.d", other)); err == nil {
			t.Errorf("instance %d: holds the other instance's pin", i)
		}
	}
}
