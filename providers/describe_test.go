package providers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// LOOM-238: what an instance offers for a new machine, for the create
// form: its targets.describe, without the host's own fields.
func TestDescribe(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, _ := h.create(t, "one", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	info, err := h.m.Describe(ctx, h.pluginID)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(info.Sizes) != 2 || info.Sizes[0].Name != "small" || info.Sizes[1].Memory != "4Gi" {
		t.Errorf("sizes = %+v", info.Sizes)
	}
	if !info.PersistentDefault || len(info.EgressOptions) != 2 || info.EgressOptions[1] != protocol.EgressNone {
		t.Errorf("persistent default %v, egress %v", info.PersistentDefault, info.EgressOptions)
	}
	if info.Image != "ghcr.io/loomux/agent:fake" || info.MaxEnvironments != 5 || info.Environments != 1 {
		t.Errorf("image %q, max %d, environments %d", info.Image, info.MaxEnvironments, info.Environments)
	}

	if _, err := h.m.Describe(ctx, "no-such-plugin"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("an unknown instance: %v, want ErrNotFound", err)
	}
	if _, err := h.pm.Disable(ctx, h.pluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Describe(ctx, h.pluginID); !errors.Is(err, providers.ErrPluginUnavailable) {
		t.Errorf("a disabled instance: %v, want ErrPluginUnavailable", err)
	}
}
