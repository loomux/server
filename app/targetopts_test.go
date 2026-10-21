package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// LOOM-202: two app instances in one process keep their tmux sockets
// apart. The socket was process-wide, set by every Build, so the first
// instance's sessions landed on the second's tmux server, where the
// second's orphan sweep would reap them as its own.
func TestBuild_TmuxSocketPerInstance(t *testing.T) {
	srv := fakeRouterServer(t)
	sockets := [2]string{fmt.Sprintf("loomux-apptest-a-%d", os.Getpid()), fmt.Sprintf("loomux-apptest-b-%d", os.Getpid())}
	var apps [2]*App
	for i, socket := range sockets {
		t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
		cfg := testConfig(t, srv.URL)
		cfg.TmuxSocket = socket
		a, err := Build(cfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		apps[i] = a
	}

	local := &registry.Target{Name: "box", Kind: registry.TargetKindLocal}
	for i, a := range apps {
		e, err := a.newExecutor(local)
		if err != nil {
			t.Fatalf("instance %d: local executor: %v", i, err)
		}
		defer e.Close()
		session := fmt.Sprintf("loomux-apptest-%d", i)
		if err := e.NewSession(context.Background(), session, "", "sleep 60"); err != nil {
			t.Fatalf("instance %d: NewSession: %v", i, err)
		}
		for j, socket := range sockets {
			on := exec.Command("tmux", "-L", socket, "has-session", "-t", session).Run() == nil
			if on != (i == j) {
				t.Errorf("instance %d's session on socket %s: %v, want %v", i, socket, on, i == j)
			}
		}
	}
}

// LOOM-202: Build scrubs the environment of its own instance's tmux
// server (LOOM-141), the socket it was given.
func TestBuild_ScrubsItsOwnTmuxSocket(t *testing.T) {
	srv := fakeRouterServer(t)
	socket := fmt.Sprintf("loomux-apptest-scrub-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "first", "sleep 60")
	start.Env = append(os.Environ(), "LOOMUX_SCRUBTEST_LEAK=1")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	cfg := testConfig(t, srv.URL)
	cfg.TmuxSocket = socket
	a, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.TmuxSocket() != socket {
		t.Errorf("TmuxSocket() = %q, want %q", a.TmuxSocket(), socket)
	}
	env, err := exec.Command("tmux", "-L", socket, "show-environment", "-g").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	if strings.Contains(string(env), "LOOMUX_SCRUBTEST_LEAK") {
		t.Errorf("the instance's tmux server still has the leaked variable:\n%s", env)
	}
}

// A socket name tmux's command line can't take is refused before the
// store opens, so nothing is left to clean up (LOOM-202).
func TestBuild_RefusesABadTmuxSocket(t *testing.T) {
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	cfg.TmuxSocket = "bad name"
	if a, err := Build(cfg); err == nil {
		_ = a.Close()
		t.Fatal("Build accepted a tmux socket name with a space")
	}
	if _, err := os.Stat(cfg.DBPath); !os.IsNotExist(err) {
		t.Errorf("the store was opened for a bad socket name: stat %s = %v", cfg.DBPath, err)
	}
}
