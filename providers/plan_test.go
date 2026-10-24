package providers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// LOOM-216: Plan resolves a request as Create would, creating nothing.
func TestPlanResolvesDefaults(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	plan, err := h.m.Plan(ctx, providers.CreateRequest{Target: registry.Target{Name: " builds "}, PluginID: h.pluginID})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := providers.Plan{PluginID: h.pluginID, PluginName: "fake", PluginLabel: plan.PluginLabel, Name: "builds",
		Size:       protocol.Size{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"},
		Persistent: true, Egress: protocol.EgressInternet, Machines: 0, MaxMachines: 5}
	if *plan != want {
		t.Errorf("plan = %+v, want %+v", *plan, want)
	}
	if envs, _ := h.store.ListEnvironmentsByPlugin(ctx, h.pluginID); len(envs) != 0 {
		t.Errorf("Plan made %d machines", len(envs))
	}

	no := false
	plan, err = h.m.Plan(ctx, providers.CreateRequest{Target: registry.Target{Name: "tmp"}, PluginID: h.pluginID,
		Size: "medium", Persistent: &no, Egress: protocol.EgressNone})
	if err != nil || plan.Size.MemoryRequest != "3Gi" || plan.Persistent || plan.Egress != protocol.EgressNone {
		t.Errorf("plan = %+v, %v", plan, err)
	}

	var invalid *providers.InvalidError
	if _, err := h.m.Plan(ctx, providers.CreateRequest{Target: registry.Target{Name: "x"}, PluginID: h.pluginID, Size: "huge"}); !errors.As(err, &invalid) || invalid.Field != "size" {
		t.Errorf("an unknown size: %v", err)
	}
	if _, err := h.m.Plan(ctx, providers.CreateRequest{Target: registry.Target{Name: "x"}, PluginID: "no-such"}); !errors.Is(err, providers.ErrPluginUnavailable) {
		t.Errorf("an unknown plugin: %v", err)
	}
}

// With NamedTrust, egress trusted is planned and created only for a
// name the plugin's describe, asked for that name, offers it to.
func TestPlanNamedTrust(t *testing.T) {
	h := newHarness(t, map[string]any{"trusted_machines": "cc"})
	ctx := context.Background()
	req := func(name string) providers.CreateRequest {
		return providers.CreateRequest{Target: registry.Target{Name: name}, PluginID: h.pluginID,
			Egress: protocol.EgressTrusted, NamedTrust: true}
	}
	if plan, err := h.m.Plan(ctx, req("cc")); err != nil || plan.Egress != protocol.EgressTrusted {
		t.Fatalf("a listed name: %+v, %v", plan, err)
	}
	var invalid *providers.InvalidError
	if _, err := h.m.Plan(ctx, req("other")); !errors.As(err, &invalid) || invalid.Field != "egress" {
		t.Errorf("an unlisted name: %v, want an egress refusal", err)
	}
	if _, _, err := h.m.Create(ctx, req("other")); !errors.As(err, &invalid) {
		t.Errorf("Create of an unlisted name: %v, want an egress refusal", err)
	}
	if envs, _ := h.store.ListEnvironmentsByPlugin(ctx, h.pluginID); len(envs) != 0 {
		t.Errorf("a refused machine was made: %d", len(envs))
	}
	// Without NamedTrust (the API), the host leaves the name to the
	// plugin's own create, as before.
	other := req("other")
	other.NamedTrust = false
	if _, err := h.m.Plan(ctx, other); err != nil {
		t.Errorf("without NamedTrust: %v", err)
	}
	// Never trusted by default, whatever the name.
	dflt := req("cc")
	dflt.Egress = ""
	if plan, err := h.m.Plan(ctx, dflt); err != nil || plan.Egress != protocol.EgressInternet {
		t.Errorf("the default egress for a listed name: %+v, %v", plan, err)
	}
}

// Offers lists the running instances that make machines, with their
// describe; a disabled one is left out.
func TestOffers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	offers, err := h.m.Offers(ctx)
	if err != nil || len(offers) != 1 {
		t.Fatalf("offers = %+v, %v", offers, err)
	}
	o := offers[0]
	if o.PluginID != h.pluginID || o.PluginName != "fake" || !o.Persistent || !o.Ephemeral || len(o.Info.Sizes) != 2 {
		t.Errorf("offer = %+v", o)
	}
	// Describe passes a Burstable size's requests on (LOOM-241).
	if o.Info.Sizes[1].MemoryRequest != "3Gi" {
		t.Errorf("sizes = %+v", o.Info.Sizes)
	}
	if _, err := h.pm.Disable(ctx, h.pluginID); err != nil {
		t.Fatal(err)
	}
	if offers, err := h.m.Offers(ctx); err != nil || len(offers) != 0 {
		t.Errorf("offers of a disabled instance = %+v, %v", offers, err)
	}
}

// A plugin slow to describe is left out of the offers after
// OfferTimeout rather than holding up the chat that asked (LOOM-216).
func TestOffers_SlowPluginLeftOut(t *testing.T) {
	h := newHarness(t, map[string]any{"targets_mode": fake.TargetsSlowDescribe})
	h.m.OfferTimeout = 50 * time.Millisecond
	offers, err := h.m.Offers(context.Background())
	if err != nil || len(offers) != 0 {
		t.Errorf("offers = %+v, %v; want the slow plugin left out", offers, err)
	}
	// The describe the API serves isn't held to the offers' bound.
	if _, err := h.m.Describe(context.Background(), h.pluginID); err != nil {
		t.Errorf("Describe: %v", err)
	}
}
