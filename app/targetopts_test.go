package app

import (
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

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

// LOOM-199: two instances' agent sockets and public-key files don't
// share a directory. A pool clears its directory of sockets the first
// time it serves a key, so the second instance's first managed
// connection unlinked the first's live agent; and the same key id in
// both stores shared one .pub file, which tells ssh the key to use.
func TestBuild_ManagedSSHDirsPerInstance(t *testing.T) {
	srv := fakeRouterServer(t)
	var apps [2]*App
	for i := range apps {
		a, err := Build(testConfig(t, srv.URL))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		apps[i] = a
	}
	if dir := apps[0].targetOpts.Managed.KeysDir; dir == apps[1].targetOpts.Managed.KeysDir {
		t.Errorf("both instances' keys dir is %s", dir)
	}

	// The same key id in both stores, a different key in each. The
	// second socket is the second pool's first.
	var keys [2]*registry.SSHKey
	var sockets [2]string
	for i, a := range apps {
		k, err := targets.GenerateSSHKey("same-key-id", "k")
		if err != nil {
			t.Fatal(err)
		}
		if sockets[i], err = a.sshAgents.Socket(k); err != nil {
			t.Fatalf("Socket: %v", err)
		}
		keys[i] = k
	}
	if filepath.Dir(sockets[0]) == filepath.Dir(sockets[1]) {
		t.Errorf("both instances' agent sockets are in %s", filepath.Dir(sockets[0]))
	}
	// Each instance's agent still answers, with its own key.
	for i, socket := range sockets {
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keys[i].PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Errorf("instance %d's agent: %v", i, err)
			continue
		}
		got, err := agent.NewClient(conn).List()
		conn.Close()
		if err != nil || len(got) != 1 || !bytes.Equal(got[0].Marshal(), want.Marshal()) {
			t.Errorf("instance %d's agent holds %v (err %v), want its own key", i, got, err)
		}
	}
}
