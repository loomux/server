package docker

import (
	"context"
	"testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
)

var ctx = context.Background()

// The manifest is what the host reads: a valid one, naming the plugin,
// the capabilities §9 gives it (no egress policy: Docker publishes no
// port on an internal network), and a schema with the secret key.
func TestManifest(t *testing.T) {
	m, err := plugins.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if m.Name != "docker" || m.Version != Version || m.Protocol != protocol.Version {
		t.Errorf("manifest = %+v", m)
	}
	for _, c := range []string{protocol.CapTargetsCreate, protocol.CapTargetsStopStart, protocol.CapTargetsRecreate, protocol.CapTargetsPersistent, protocol.CapTargetsEphemeral, protocol.CapTargetsAttachCommands} {
		if !m.HasCapability(c) {
			t.Errorf("manifest lacks %s", c)
		}
	}
	if m.HasCapability(protocol.CapTargetsEgressPolicy) {
		t.Error("the manifest declares targets.egress_policy, which Docker can't honour (no port is published on an internal network)")
	}
	schema, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"engine", "bind_address", "agent_image"} {
		if !required(schema, f) {
			t.Errorf("%s should be required", f)
		}
	}
	if p := schema.Properties["ssh_private_key"]; p == nil || !p.Secret || p.Format != "ssh-private-key" {
		t.Errorf("ssh_private_key = %+v", p)
	}
	if p := schema.Properties["proxy"]; p == nil || p.Format != "socks5-url" {
		t.Errorf("proxy = %+v", p)
	}
	d, err := New().Describe(ctx)
	if err != nil || !d.Equal(m) {
		t.Errorf("Describe = %+v, %v", d, err)
	}
}

func required(s *plugins.Schema, name string) bool {
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}
