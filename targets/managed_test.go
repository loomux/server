package targets_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/executortest"
	"github.com/Loomux/server/targets/sshtest"
)

// TestMain lets this test binary stand in for loomuxd as the ssh
// ProxyCommand: ssh runs it as `<binary> -ssh-proxy-connect PROXY HOST PORT`.
func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == targets.RelayFlag {
		if err := targets.RelaySOCKS5(os.Args[2], os.Args[3], os.Args[4], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// managedSetup is a loopback SSH server with a Loomux-generated key
// authorized on it, managed SSH configured to use that key, and a
// managed target for the server, pinned to its host key.
type managedSetup struct {
	server *sshtest.Server
	key    *registry.SSHKey
	target *registry.Target
}

func setupManaged(t *testing.T, proxy string) *managedSetup {
	t.Helper()
	server := sshtest.Start(t)
	key, err := targets.GenerateSSHKey("key-"+strconv.Itoa(server.Port), "test")
	if err != nil {
		t.Fatal(err)
	}
	server.Authorize(t, key.PublicKey)
	base := shortTempDir(t)
	pool := targets.NewAgentPool(filepath.Join(base, "agents"))
	t.Cleanup(func() { pool.Close() })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = targets.SetManagedSSH(&targets.ManagedSSH{
		Keys: func(_ context.Context, id string) (*registry.SSHKey, error) {
			if id != key.ID {
				return nil, registry.ErrNotFound
			}
			return key, nil
		},
		Agents:  pool,
		KeysDir: filepath.Join(base, "keys"),
		Proxy:   proxy,
		Relay:   self,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = targets.SetManagedSSH(nil) })
	targets.SetKnownHostsDir(filepath.Join(base, "known_hosts.d"))
	tgt := &registry.Target{
		ID: "managed-" + strconv.Itoa(server.Port), Name: "managed", Kind: registry.TargetKindRemote,
		Host: server.Host, User: "loomux-test", SSHPort: server.Port, SSHKeyRef: key.ID,
		HostKeys: server.KnownHostsLine(),
	}
	if proxy == "" {
		tgt.SSHProxy = registry.SSHProxyNone
	}
	return &managedSetup{server: server, key: key, target: tgt}
}

func newManagedExecutor(t *testing.T, tgt *registry.Target) targets.TargetExecutor {
	t.Helper()
	e, err := targets.NewExecutor(tgt)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// LOOM-138: a managed target runs the whole executor contract with the
// key Loomux generated, served by its agent, and no ssh_config.
func TestManagedExecutor_Contract(t *testing.T) {
	s := setupManaged(t, "")
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		return newManagedExecutor(t, s.target)
	})
}

// Through the server's SOCKS5 proxy, relayed by loomuxd itself.
func TestManagedExecutor_ThroughSOCKS5(t *testing.T) {
	socks := startSOCKS5(t)
	s := setupManaged(t, socks.addr)
	e := newManagedExecutor(t, s.target)
	out, err := e.RunOnce(context.Background(), "echo through-the-proxy")
	if err != nil || !strings.Contains(out, "through-the-proxy") {
		t.Fatalf("RunOnce = %q, %v", out, err)
	}
	want := net.JoinHostPort(s.server.Host, strconv.Itoa(s.server.Port))
	if got := socks.connects(); len(got) == 0 || got[0] != want {
		t.Errorf("SOCKS5 CONNECTs = %v, want %s", got, want)
	}
}

// Without a pin a managed target is refused before ssh ever runs: there
// is no known_hosts to fall back to.
func TestManagedExecutor_RequiresAPin(t *testing.T) {
	s := setupManaged(t, "")
	s.target.HostKeys = ""
	_, err := targets.NewExecutor(s.target)
	var ue *targets.UnreachableError
	if !errors.As(err, &ue) || ue.Failure != targets.SSHHostKeyUnknown {
		t.Fatalf("NewExecutor without a pin = %v, want host_key_unknown", err)
	}
	if strings.Contains(ue.Hint(), "SSH secret") {
		t.Errorf("hint for a managed target mentions the SSH secret: %s", ue.Hint())
	}
}

// A pin that isn't the host's key is a changed host key, not a connection.
func TestManagedExecutor_WrongPinRefused(t *testing.T) {
	s := setupManaged(t, "")
	other := sshtest.Start(t)
	s.target.HostKeys = fmt.Sprintf("[%s]:%d %s", s.server.Host, s.server.Port, other.HostKey)
	e := newManagedExecutor(t, s.target)
	_, err := e.RunOnce(context.Background(), "true")
	var ue *targets.UnreachableError
	if !errors.As(err, &ue) || ue.Failure != targets.SSHHostKeyChanged {
		t.Fatalf("RunOnce with a wrong pin = %v, want host_key_changed", err)
	}
}

// A key the target hasn't authorized is an auth failure whose hint says
// what to do.
func TestManagedExecutor_UnauthorizedKey(t *testing.T) {
	s := setupManaged(t, "")
	stranger := sshtest.Start(t) // has never seen s.key
	s.target.SSHPort, s.target.HostKeys = stranger.Port, stranger.KnownHostsLine()
	e := newManagedExecutor(t, s.target)
	_, err := e.RunOnce(context.Background(), "true")
	var ue *targets.UnreachableError
	if !errors.As(err, &ue) || ue.Failure != targets.SSHAuthFailed {
		t.Fatalf("RunOnce with an unauthorized key = %v, want auth_failed", err)
	}
	if !strings.Contains(ue.Hint(), "authorized_keys") {
		t.Errorf("hint = %q", ue.Hint())
	}
}

func TestManagedExecutor_NotConfigured(t *testing.T) {
	_ = targets.SetManagedSSH(nil)
	tgt := &registry.Target{ID: "t", Name: "t", Kind: registry.TargetKindRemote, Host: "h", User: "u", SSHKeyRef: "k", HostKeys: "h ssh-ed25519 AAAA"}
	if _, err := targets.NewExecutor(tgt); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("NewExecutor without managed SSH = %v", err)
	}
}

// A managed target's host key is scanned the same way it's reached.
func TestScanHostKey_Managed(t *testing.T) {
	socks := startSOCKS5(t)
	s := setupManaged(t, socks.addr)
	s.target.HostKeys = ""
	keys, err := targets.ScanHostKey(context.Background(), s.target)
	if err != nil || len(keys) != 1 || !strings.HasSuffix(keys[0].Line, s.server.HostKey) {
		t.Fatalf("ScanHostKey = %+v, %v", keys, err)
	}
	if len(socks.connects()) == 0 {
		t.Error("the scan didn't go through the proxy")
	}
}

func TestSetManagedSSH_Validates(t *testing.T) {
	defer func() { _ = targets.SetManagedSSH(nil) }()
	pool := targets.NewAgentPool(shortTempDir(t))
	base := func() targets.ManagedSSH {
		return targets.ManagedSSH{Keys: func(context.Context, string) (*registry.SSHKey, error) { return nil, nil },
			Agents: pool, KeysDir: shortTempDir(t), Relay: "/usr/local/bin/loomuxd"}
	}
	for name, mutate := range map[string]func(*targets.ManagedSSH){
		"proxy with a command": func(m *targets.ManagedSSH) { m.Proxy = "127.0.0.1:1055; id" },
		"proxy without port":   func(m *targets.ManagedSSH) { m.Proxy = "127.0.0.1" },
		"proxy with a token":   func(m *targets.ManagedSSH) { m.Proxy = "%h:1055" },
		"relay with a space":   func(m *targets.ManagedSSH) { m.Relay = "/tmp/a b" },
		"relay with a %":       func(m *targets.ManagedSSH) { m.Relay = "/tmp/%d" },
		"relay not absolute":   func(m *targets.ManagedSSH) { m.Relay = "loomuxd" },
		"no key loader":        func(m *targets.ManagedSSH) { m.Keys = nil },
	} {
		m := base()
		mutate(&m)
		if err := targets.SetManagedSSH(&m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := base()
	m.Proxy = "127.0.0.1:1055"
	if err := targets.SetManagedSSH(&m); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
}

func TestRelaySOCKS5_RefusesBadDestinations(t *testing.T) {
	for _, d := range [][2]string{{"h", "0"}, {"h", "70000"}, {"h", "22x"}, {"", "22"}, {"a b", "22"}, {"-x", "22"}} {
		if err := targets.RelaySOCKS5("127.0.0.1:1", d[0], d[1], strings.NewReader(""), io.Discard); err == nil {
			t.Errorf("RelaySOCKS5 to %q:%q accepted", d[0], d[1])
		}
	}
}

// socks5 is a minimal no-auth SOCKS5 server recording what it was asked
// to CONNECT to.
type socks5 struct {
	addr string
	mu   sync.Mutex
	seen []string
}

func (s *socks5) connects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func startSOCKS5(t *testing.T) *socks5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &socks5{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *socks5) handle(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(c, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		n := int(buf[0])
		io.ReadFull(c, buf[:n])
		host = string(buf[:n])
	case 4:
		io.ReadFull(c, buf[:16])
		host = net.IP(buf[:16]).String()
	}
	io.ReadFull(c, buf[:2])
	dest := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(buf[:2]))))
	s.mu.Lock()
	s.seen = append(s.seen, dest)
	s.mu.Unlock()
	up, err := net.Dial("tcp", dest)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}
