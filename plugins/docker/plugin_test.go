package docker

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

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

const testInstance = "11111111-2222-3333-4444-555555555555"

// newTestPlugin is a plugin with short waits and no log output.
func newTestPlugin() *Plugin {
	p := New()
	p.Logger = log.New(io.Discard, "", 0)
	p.CreateWait = 2 * time.Second
	p.CreateTimeout = 10 * time.Second
	p.OpTTL = time.Minute
	return p
}

// configureParams is what the host sends at configure: cfg with the
// schema's defaults, validated against it.
func configureParams(t *testing.T, cfg map[string]any) protocol.ConfigureParams {
	t.Helper()
	m, err := plugins.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["agent_image"]; !ok {
		cfg["agent_image"] = "ghcr.io/loomux/agent:test"
	}
	cfg = s.ApplyDefaults(cfg)
	if err := s.Validate(cfg); err != nil {
		t.Fatalf("config isn't valid for the schema: %v", err)
	}
	return protocol.ConfigureParams{Config: cfg, Host: protocol.HostInfo{Version: "test", InstanceID: testInstance}}
}

// spec is a machine's spec as the host sends it.
func spec(id string) protocol.EnvironmentSpec {
	return protocol.EnvironmentSpec{
		ID: id, TargetID: "target-" + id, Name: "builds for project x", Size: "small", Persistent: true, Egress: protocol.EgressInternet,
		Image: "ghcr.io/loomux/agent:test",
		SSH: protocol.SSHBootstrap{
			AuthorizedKey:  "no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestTestTestTestTestTestTestTestTestTestKey loomux-builds",
			HostPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nTESTKEY\n-----END OPENSSH PRIVATE KEY-----\n",
			HostPublicKey:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostHostHostHostHostHostHostHostHostHostHost",
			Port:           2222,
		},
		Labels: map[string]string{protocol.LabelInstance: testInstance, protocol.LabelTarget: "target-" + id, protocol.LabelEnvironment: id},
	}
}
