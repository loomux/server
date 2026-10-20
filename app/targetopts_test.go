package app

import (
	"errors"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// LOOM-199: two app instances in one process keep their managed SSH and
// local-target settings apart. Both used to be process-wide, set by every
// Build, so the second instance's proxy and agents, and its local-target
// switch, became the first's too.
func TestBuild_TargetOptionsPerInstance(t *testing.T) {
	srv := fakeRouterServer(t)
	build := func(proxy string, localOff bool) *App {
		cfg := testConfig(t, srv.URL)
		cfg.SSHProxy, cfg.LocalTargetsOff = proxy, localOff
		a, err := Build(cfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	first := build("127.0.0.1:1055", true)
	second := build("10.9.8.7:1080", false)

	for _, c := range []struct {
		name     string
		app      *App
		proxy    string
		localOff bool
	}{{"first", first, "127.0.0.1:1055", true}, {"second", second, "10.9.8.7:1080", false}} {
		m := c.app.targetOpts.Managed
		if m == nil || m.Proxy != c.proxy || m.Agents != c.app.sshAgents {
			t.Errorf("%s: managed SSH = %+v, want its own proxy %s and agent pool", c.name, m, c.proxy)
		}
		if c.app.LocalTargets() == c.localOff {
			t.Errorf("%s: LocalTargets() = %v, want %v", c.name, c.app.LocalTargets(), !c.localOff)
		}
		e, err := c.app.newExecutor(&registry.Target{Name: "box", Kind: registry.TargetKindLocal})
		if c.localOff != errors.Is(err, targets.ErrLocalTargetsOff) {
			t.Errorf("%s: local executor err = %v, want local targets off = %v", c.name, err, c.localOff)
		}
		if e != nil {
			e.Close()
		}
	}

	// Closing one leaves the other's managed SSH as it was.
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if m := first.targetOpts.Managed; m == nil || m.Agents != first.sshAgents {
		t.Errorf("after the other instance closed, managed SSH = %+v", m)
	}
}
