package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// LOOM-178: the app wires the providers manager: a machine created
// through it comes up, is registered as a managed target with a pinned
// host key, and survives a restart as a machine (reconciled, not
// recreated).
func TestBuild_MachinesComeUp(t *testing.T) {
	bundle := buildFakePlugin(t)
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	cfg.MasterKey = []byte("01234567890123456789012345678901")
	cfg.PluginBundleDir = bundle
	ctx := context.Background()

	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	v, err := app.Plugins().Install(ctx, plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "fake"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	app.Machines().PollInterval = 20 * time.Millisecond
	target, env, err := app.Machines().Create(ctx, providers.CreateRequest{Target: registry.Target{Name: "builds"}, PluginID: v.ID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !target.Managed() || !strings.HasPrefix(target.HostKeys, "[lx-"+env.ID+".fake.invalid]:2222 ") || target.WorkspaceRoot != "/data/work" {
		t.Errorf("target = %+v", target)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		e, err := app.Machines().Get(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Status == registry.EnvironmentRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine never ran: %s (%s)", e.Status, e.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The probe ran against the (unreachable, fake) address and recorded
	// a health row: the machine is a target like any other.
	deadline = time.Now().Add(10 * time.Second)
	for {
		if _, err := app.Store().GetTargetHealth(ctx, target.ID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no health probe recorded for the machine")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	app2, err := Build(cfg)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	defer app2.Close()
	deadline = time.Now().Add(10 * time.Second)
	for {
		e, err := app2.Machines().Get(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Status == registry.EnvironmentRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a restart the machine is %s (%s)", e.Status, e.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
