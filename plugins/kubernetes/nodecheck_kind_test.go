//go:build kind

package kubernetes

import (
	"os"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
)

// TestKindNodeCanary runs canaries against the kind cluster of TestKind
// (kind_test.go), as the plugin's ServiceAccount: one the scheduler
// places, then one pinned to the node it landed on, which the kubelet
// there runs and the engine there confines (LOOM-208), and one pinned to
// a node that doesn't exist, which never runs and never says
// "enforced". kind has one node, so a machine there is confirmed by the
// check's own canary; this is where a pinned one runs for real.
func TestKindNodeCanary(t *testing.T) {
	kcPath := os.Getenv("LOOMUX_KIND_KUBECONFIG")
	if kcPath == "" {
		t.Skip("LOOMUX_KIND_KUBECONFIG not set")
	}
	kubeconfig, err := os.ReadFile(kcPath)
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("LOOMUX_KIND_AGENT_IMAGE")
	if image == "" {
		image = "loomux-agent:test"
	}
	p := New()
	p.CanaryTimeout = 90 * time.Second
	if err := p.Configure(ctx, protocol.ConfigureParams{
		Config: baseConfig(t, map[string]any{"storage_class": "standard", "agent_image": image, "kubeconfig": string(kubeconfig)}),
		Host:   protocol.HostInfo{Version: "kind", InstanceID: "kind-node-canary"},
	}); err != nil {
		t.Fatal(err)
	}
	want := enforcementNotEnforced
	if os.Getenv("LOOMUX_KIND_ENFORCED") == "1" {
		want = enforcementEnforced
	}
	state, detail, node, _ := p.canary(ctx, p.cfg, p.client, p.cfg.apiHost, "kind-node-canary", "")
	t.Logf("scheduled: %s on %q (%s)", state, node, detail)
	if state != want || node == "" {
		t.Fatalf("the scheduler's canary: %s on %q (%s), want %s on a node", state, node, detail, want)
	}
	state, detail, ranOn, _ := p.canary(ctx, p.cfg, p.client, p.cfg.apiHost, "kind-node-canary", node)
	t.Logf("pinned to %s: %s on %q (%s)", node, state, ranOn, detail)
	if state != want || ranOn != node {
		t.Errorf("pinned to %s: %s on %q (%s), want %s there", node, state, ranOn, detail, want)
	}
	p.CanaryTimeout = 20 * time.Second
	state, detail, _, _ = p.canary(ctx, p.cfg, p.client, p.cfg.apiHost, "kind-node-canary", "no-such-node")
	t.Logf("pinned to no-such-node: %s (%s)", state, detail)
	if state != enforcementFailed {
		t.Errorf("pinned to a node that doesn't exist: %s (%s), want failed", state, detail)
	}
}
