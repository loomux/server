package plugintest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// RunTargets is the conformance suite for the targets.* group
// (docs/design/target-providers.md §12). Run includes it for a plugin
// whose manifest declares targets.create. Each case launches a fresh
// plugin, configures it, and drives one machine through its life; the
// id is prefixed "plugintest-" so a real plugin can recognise and clean
// up what the suite leaves behind if a case fails half-way.
func RunTargets(t *testing.T, launch Launch, o Options) {
	if o.CallTimeout == 0 {
		o.CallTimeout = 30 * time.Second
	}
	t.Run("TargetsDescribe", func(t *testing.T) { testTargetsDescribe(t, launch, o) })
	t.Run("TargetsLifecycle", func(t *testing.T) { testTargetsLifecycle(t, launch, o) })
	t.Run("TargetsUnknownID", func(t *testing.T) { testTargetsUnknownID(t, launch, o) })
	t.Run("TargetsCreateRequiresID", func(t *testing.T) { testTargetsCreateRequiresID(t, launch, o) })
}

// testSpec is the machine the suite creates.
func testSpec(id string, info protocol.TargetsInfo) protocol.EnvironmentSpec {
	size := "small"
	if len(info.Sizes) > 0 {
		size = info.Sizes[0].Name
	}
	egress := protocol.EgressInternet
	if len(info.EgressOptions) > 0 {
		egress = info.EgressOptions[0]
	}
	return protocol.EnvironmentSpec{
		ID: id, TargetID: "plugintest-target", Name: "plugintest", Size: size, Persistent: info.PersistentDefault,
		Egress: egress, Image: info.Image,
		SSH: protocol.SSHBootstrap{
			AuthorizedKey:  "no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlugintestPlugintestPlugintestPlugintestKey plugintest",
			HostPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nplugintest\n-----END OPENSSH PRIVATE KEY-----\n",
			HostPublicKey:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlugintestHostKeyPlugintestHostKeyPlugintest",
			Port:           info.Port,
		},
		Labels: map[string]string{protocol.LabelInstance: "plugintest-instance", protocol.LabelTarget: "plugintest-target", protocol.LabelEnvironment: id},
	}
}

func describeTargets(t *testing.T, conn *rpc.Conn, o Options) protocol.TargetsInfo {
	t.Helper()
	var info protocol.TargetsInfo
	if err := call(t, conn, o, protocol.MethodTargetsDescribe, nil, &info); err != nil {
		t.Fatalf("targets.describe: %v", err)
	}
	return info
}

func checkEnvironment(t *testing.T, env protocol.Environment, id string) {
	t.Helper()
	if env.ID != id {
		t.Errorf("environment id = %q, want %q", env.ID, id)
	}
	known := false
	for _, s := range protocol.EnvironmentStatuses {
		if env.Status == s {
			known = true
		}
	}
	if !known {
		t.Errorf("environment status %q isn't one the host knows", env.Status)
	}
	if env.Address.Host == "" || env.Address.Port < 1 || env.Address.Port > 65535 {
		t.Errorf("environment address = %+v", env.Address)
	}
	if strings.ContainsAny(env.Address.Host, " \t\r\n;$`'\"\\@/") || strings.HasPrefix(env.Address.Host, "-") {
		t.Errorf("environment host %q would never pass the host's grammar", env.Address.Host)
	}
	switch env.Address.Proxy {
	case protocol.SSHProxyNone, protocol.SSHProxyDefault:
	default:
		t.Errorf("environment proxy %q isn't none or default", env.Address.Proxy)
	}
}

func testTargetsDescribe(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	if !m.HasCapability(protocol.CapTargetsCreate) {
		t.Skip("the manifest doesn't declare targets.create")
	}
	configure(t, conn, o, m)
	info := describeTargets(t, conn, o)
	if len(info.Sizes) == 0 {
		t.Error("targets.describe should offer at least one size")
	}
	for _, s := range info.Sizes {
		if s.Name == "" {
			t.Errorf("a size needs a name: %+v", s)
		}
	}
	if info.Port < 1 || info.Port > 65535 || info.User == "" {
		t.Errorf("targets.describe must say the sshd user and port: %+v", info)
	}
	if !strings.Contains(info.AddressTemplate, "{id}") {
		t.Errorf("address_template must contain {id}: %q", info.AddressTemplate)
	}
	switch info.SSHProxy {
	case protocol.SSHProxyNone, protocol.SSHProxyDefault:
	default:
		t.Errorf("ssh_proxy %q isn't none or default", info.SSHProxy)
	}
	if m.HasCapability(protocol.CapTargetsEgressPolicy) && len(info.EgressOptions) == 0 {
		t.Error("a plugin declaring targets.egress_policy should offer egress options")
	}
}

func testTargetsLifecycle(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	if !m.HasCapability(protocol.CapTargetsCreate) {
		t.Skip("the manifest doesn't declare targets.create")
	}
	configure(t, conn, o, m)
	info := describeTargets(t, conn, o)
	// An id a plugin can turn into a DNS label: a long test name is cut,
	// and never left ending in a dash.
	id := "plugintest-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if len(id) > 32 {
		id = id[:32]
	}
	id = strings.TrimRight(id, "-")
	spec := testSpec(id, info)
	defer func() { _ = call(t, conn, o, protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil) }()

	var first, second protocol.Environment
	if err := call(t, conn, o, protocol.MethodTargetsCreate, spec, &first); err != nil {
		t.Fatalf("targets.create: %v", err)
	}
	checkEnvironment(t, first, id)
	if err := call(t, conn, o, protocol.MethodTargetsCreate, spec, &second); err != nil {
		t.Fatalf("targets.create again: %v", err)
	}
	if second.ID != first.ID || second.Address != first.Address {
		t.Errorf("targets.create isn't idempotent: %+v then %+v", first, second)
	}

	var got protocol.Environment
	if err := call(t, conn, o, protocol.MethodTargetsGet, protocol.IDParams{ID: id}, &got); err != nil {
		t.Fatalf("targets.get: %v", err)
	}
	checkEnvironment(t, got, id)

	var list protocol.ListResult
	if err := call(t, conn, o, protocol.MethodTargetsList, nil, &list); err != nil {
		t.Fatalf("targets.list: %v", err)
	}
	found := false
	for _, e := range list.Environments {
		if e.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("targets.list doesn't include the machine just created: %+v", list.Environments)
	}

	var health protocol.EnvironmentHealth
	if err := call(t, conn, o, protocol.MethodTargetsHealth, protocol.IDParams{ID: id}, &health); err != nil {
		t.Fatalf("targets.health: %v", err)
	}
	if health.Status == "" {
		t.Error("targets.health should report a status")
	}

	if m.HasCapability(protocol.CapTargetsStopStart) {
		if err := call(t, conn, o, protocol.MethodTargetsStop, protocol.IDParams{ID: id}, nil); err != nil {
			t.Fatalf("targets.stop: %v", err)
		}
		if err := call(t, conn, o, protocol.MethodTargetsStop, protocol.IDParams{ID: id}, nil); err != nil {
			t.Errorf("targets.stop twice: %v", err)
		}
		if err := call(t, conn, o, protocol.MethodTargetsStart, protocol.IDParams{ID: id}, nil); err != nil {
			t.Fatalf("targets.start: %v", err)
		}
	}
	if m.HasCapability(protocol.CapTargetsRecreate) {
		var re protocol.Environment
		if err := call(t, conn, o, protocol.MethodTargetsRecreate, protocol.RecreateParams{ID: id, Spec: spec}, &re); err != nil {
			t.Fatalf("targets.recreate: %v", err)
		}
		checkEnvironment(t, re, id)
		if re.Address != first.Address {
			t.Errorf("targets.recreate changed the address: %+v → %+v", first.Address, re.Address)
		}
	}
	if m.HasCapability(protocol.CapTargetsAttachCommands) {
		var att protocol.AttachResult
		if err := call(t, conn, o, protocol.MethodTargetsAttachCommands, protocol.AttachParams{ID: id, Session: "loomux-plugintest"}, &att); err != nil {
			t.Fatalf("targets.attach_commands: %v", err)
		}
		for _, c := range att.Commands {
			if c.Via == "" || c.Command == "" || len(c.Command) > 512 || strings.ContainsAny(c.Command, "\r\n\x00") {
				t.Errorf("attach command outside the grammar: %+v", c)
			}
		}
	}

	if err := call(t, conn, o, protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("targets.destroy: %v", err)
	}
	if err := call(t, conn, o, protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil); err != nil {
		t.Errorf("targets.destroy twice: %v", err)
	}
	err := call(t, conn, o, protocol.MethodTargetsGet, protocol.IDParams{ID: id}, &got)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeNotFound {
		t.Errorf("targets.get after destroy: want not_found, got %v", err)
	}
}

func testTargetsUnknownID(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	if !m.HasCapability(protocol.CapTargetsCreate) {
		t.Skip("the manifest doesn't declare targets.create")
	}
	configure(t, conn, o, m)
	var rpcErr *rpc.Error
	err := call(t, conn, o, protocol.MethodTargetsGet, protocol.IDParams{ID: "plugintest-never-made"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeNotFound {
		t.Errorf("targets.get of an unknown id: want not_found, got %v", err)
	}
	if err := call(t, conn, o, protocol.MethodTargetsDestroy, protocol.IDParams{ID: "plugintest-never-made"}, nil); err != nil {
		t.Errorf("targets.destroy of an unknown id should be a no-op: %v", err)
	}
}

func testTargetsCreateRequiresID(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	if !m.HasCapability(protocol.CapTargetsCreate) {
		t.Skip("the manifest doesn't declare targets.create")
	}
	configure(t, conn, o, m)
	var rpcErr *rpc.Error
	err := call(t, conn, o, protocol.MethodTargetsCreate, map[string]any{"name": "x"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeInvalidParams {
		t.Errorf("targets.create without an id: want invalid_params, got %v", err)
	}
}
