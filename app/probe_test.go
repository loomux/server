package app

import (
	"context"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
)

// LOOM-86: the app probes every target's health in the background, on
// its own, and records it.
func TestBuild_ProbesTargetHealthPeriodically(t *testing.T) {
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	cfg.TargetProbeInterval = 50 * time.Millisecond

	seed, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := seed.CreateTarget(context.Background(), &registry.Target{ID: "t1", Name: "local", Kind: registry.TargetKindLocal,
		WorkspaceRoot: t.TempDir()}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	_ = seed.Close()

	a, err := build(cfg, router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := a.store.GetTargetHealth(context.Background(), "t1")
		if err == nil {
			if !h.Reachable || h.DiskFreeBytes <= 0 {
				t.Errorf("health = %+v, want the local target reachable with disk free measured", h)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no health recorded within 10s: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
