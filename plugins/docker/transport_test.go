package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/targets/sshtest"
)

// dialStdioHook serves "docker system dial-stdio" by piping the session
// to the engine's unix socket, as the real command does on a docker
// host. Anything else is refused as a shell would with no docker.
func dialStdioHook(t *testing.T, socket string, calls *atomic.Int32) func(string, io.Reader, io.Writer, io.Writer) (int, bool) {
	return func(command string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		if command != "docker system dial-stdio" {
			io.WriteString(stderr, "sh: "+command+": not found\n")
			return 127, true
		}
		calls.Add(1)
		conn, err := net.Dial("unix", socket)
		if err != nil {
			io.WriteString(stderr, err.Error())
			return 1, true
		}
		defer conn.Close()
		done := make(chan struct{})
		go func() { io.Copy(conn, stdin); conn.(*net.UnixConn).CloseWrite(); close(done) }()
		io.Copy(stdout, conn)
		<-done
		return 0, true
	}
}

// sshConfigMap is a configuration reaching s as the plugin would: its
// own key (sshtest's identity), the host key pinned unless unpinned.
func sshConfigMap(t *testing.T, s *sshtest.Server, pin bool, extra map[string]any) map[string]any {
	t.Helper()
	key, err := os.ReadFile(s.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"engine": "ssh://tester@" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), "ssh_private_key": string(key), "bind_address": "127.0.0.1", "agent_image": "ghcr.io/loomux/agent:test"}
	if pin {
		m["ssh_host_key"] = s.HostKey
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// sshConfig is sshConfigMap parsed.
func sshConfig(t *testing.T, s *sshtest.Server, pin bool, extra map[string]any) Config {
	t.Helper()
	cfg, err := parseConfig(sshConfigMap(t, s, pin, extra))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSSHDialStdio(t *testing.T) {
	socket := serveUnix(t, pingHandler())
	s := sshtest.Start(t)
	var calls atomic.Int32
	s.Exec = dialStdioHook(t, socket, &calls)
	d := newSSHDialer(sshConfig(t, s, true, nil))
	defer d.Close()
	e := newEngine(d.Dial)
	defer e.Close()
	for i := 0; i < 3; i++ {
		if v, err := e.ping(ctx); err != nil || v != "1.47" {
			t.Fatalf("ping %d = %q, %v", i, v, err)
		}
	}
	if calls.Load() < 1 {
		t.Error("dial-stdio never ran")
	}
	// The connection is reused: one SSH client, few sessions.
	if n := calls.Load(); n > 2 {
		t.Errorf("%d dial-stdio sessions for three requests; keep-alive isn't working", n)
	}
	// A lost SSH connection is made again on the next request.
	d.mu.Lock()
	d.client.Close()
	d.mu.Unlock()
	e.closeIdle()
	if v, err := e.ping(ctx); err != nil || v != "1.47" {
		t.Errorf("ping after the connection dropped = %q, %v", v, err)
	}
}

func TestSSHRemoteDockerMissing(t *testing.T) {
	s := sshtest.Start(t)
	s.Exec = func(command string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		io.WriteString(stderr, "bash: docker: command not found\n")
		return 127, true
	}
	d := newSSHDialer(sshConfig(t, s, true, nil))
	defer d.Close()
	e := newEngine(d.Dial)
	defer e.Close()
	_, err := e.ping(ctx)
	if err == nil || !strings.Contains(err.Error(), "dial-stdio") || !strings.Contains(err.Error(), "command not found") {
		t.Errorf("want the remote command's failure, got %v", err)
	}
}

func TestSSHHostKey(t *testing.T) {
	socket := serveUnix(t, pingHandler())
	s := sshtest.Start(t)
	var calls atomic.Int32
	s.Exec = dialStdioHook(t, socket, &calls)

	t.Run("unpinned", func(t *testing.T) {
		d := newSSHDialer(sshConfig(t, s, false, nil))
		defer d.Close()
		if _, err := d.Dial(ctx); !errors.Is(err, errHostKeyUnpinned) {
			t.Errorf("Dial without a pin = %v, want errHostKeyUnpinned", err)
		}
		pub, err := d.Scan(ctx)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))); line != s.HostKey {
			t.Errorf("scanned %q, want %q", line, s.HostKey)
		}
		if fp := ssh.FingerprintSHA256(pub); !strings.HasPrefix(fp, "SHA256:") {
			t.Errorf("fingerprint %q", fp)
		}
		if calls.Load() != 0 {
			t.Error("a scan ran a command on the host")
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		_, other := testKey(t)
		d := newSSHDialer(sshConfig(t, s, false, map[string]any{"ssh_host_key": other}))
		defer d.Close()
		if _, err := d.Dial(ctx); !errors.Is(err, errHostKeyMismatch) {
			t.Errorf("Dial with the wrong pin = %v, want errHostKeyMismatch", err)
		}
		if calls.Load() != 0 {
			t.Error("a mismatched host key still ran a command on the host")
		}
	})
	t.Run("unauthorized key", func(t *testing.T) {
		stranger, _ := testKey(t)
		d := newSSHDialer(sshConfig(t, s, true, map[string]any{"ssh_private_key": stranger}))
		defer d.Close()
		if _, err := d.Dial(ctx); !errors.Is(err, errAuth) {
			t.Errorf("Dial with a key the host doesn't know = %v, want errAuth", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		d := newSSHDialer(sshConfig(t, s, true, map[string]any{"engine": "ssh://tester@127.0.0.1:1"}))
		defer d.Close()
		if _, err := d.Dial(ctx); err == nil || errors.Is(err, errAuth) || errors.Is(err, errHostKeyMismatch) || errors.Is(err, errHostKeyUnpinned) {
			t.Errorf("Dial of a closed port = %v, want a plain connection error", err)
		}
	})
}

// socks5Server is a minimal SOCKS5 server (no auth, CONNECT only): what
// the userspace Tailscale sidecar offers the plugin.
func socks5Server(t *testing.T, connects *atomic.Int32) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var hdr [2]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 {
					return
				}
				methods := make([]byte, hdr[1])
				io.ReadFull(c, methods)
				c.Write([]byte{5, 0})
				var req [4]byte
				if _, err := io.ReadFull(c, req[:]); err != nil || req[1] != 1 {
					return
				}
				var host string
				switch req[3] {
				case 1:
					var ip [4]byte
					io.ReadFull(c, ip[:])
					host = net.IP(ip[:]).String()
				case 3:
					var n [1]byte
					io.ReadFull(c, n[:])
					b := make([]byte, n[0])
					io.ReadFull(c, b)
					host = string(b)
				default:
					return
				}
				var port [2]byte
				io.ReadFull(c, port[:])
				target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))))
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer target.Close()
				connects.Add(1)
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(target, c)
				io.Copy(c, target)
			}()
		}
	}()
	return l.Addr().String()
}

func TestSSHThroughSOCKS5(t *testing.T) {
	socket := serveUnix(t, pingHandler())
	s := sshtest.Start(t)
	var calls, connects atomic.Int32
	s.Exec = dialStdioHook(t, socket, &calls)
	proxy := socks5Server(t, &connects)
	d := newSSHDialer(sshConfig(t, s, true, map[string]any{"proxy": "socks5://" + proxy}))
	defer d.Close()
	e := newEngine(d.Dial)
	defer e.Close()
	if v, err := e.ping(ctx); err != nil || v != "1.47" {
		t.Fatalf("ping through the proxy = %q, %v", v, err)
	}
	if connects.Load() != 1 {
		t.Errorf("the proxy saw %d CONNECTs, want 1", connects.Load())
	}
}

func TestDialFuncFor(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"engine": "unix:///var/run/docker.sock", "bind_address": "127.0.0.1", "agent_image": "x", "ssh_proxy": protocol.SSHProxyNone})
	if err != nil {
		t.Fatal(err)
	}
	d := newDialer(cfg)
	defer d.Close()
	if _, ok := d.(*unixDialer); !ok {
		t.Errorf("unix engine → %T", d)
	}
	s := sshtest.Start(t)
	d2 := newDialer(sshConfig(t, s, true, nil))
	defer d2.Close()
	if _, ok := d2.(*sshDialer); !ok {
		t.Errorf("ssh engine → %T", d2)
	}
	if _, err := d2.Dial(context.Background()); err != nil {
		// sshtest runs the command in a shell: no docker here, which is
		// the shell's failure, not the dial's.
		t.Logf("dial through the shell: %v", err)
	}
}

// relay forwards TCP connections to a target and can freeze the ones
// already open (they stay connected but carry nothing more), as a
// tailnet path change or a NAT timeout does; new connections work.
type relay struct {
	l      net.Listener
	target string
	stop   chan struct{}
	mu     sync.Mutex
	frozen []chan struct{}
}

func newRelay(t *testing.T, target string) *relay {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{l: l, target: target, stop: make(chan struct{})}
	t.Cleanup(func() { close(r.stop); l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go r.handle(c)
		}
	}()
	return r
}

func (r *relay) handle(c net.Conn) {
	defer c.Close()
	up, err := net.Dial("tcp", r.target)
	if err != nil {
		return
	}
	defer up.Close()
	frozen := make(chan struct{})
	r.mu.Lock()
	r.frozen = append(r.frozen, frozen)
	r.mu.Unlock()
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 4096)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				select {
				case <-frozen:
					<-r.stop
					return
				default:
				}
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	done := make(chan struct{})
	go func() { pipe(up, c); close(done) }()
	pipe(c, up)
	<-done
}

// freeze stops every connection open now, keeping it open.
func (r *relay) freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frozen {
		close(f)
	}
	r.frozen = nil
}

func (r *relay) addr() string { return r.l.Addr().String() }

// A connection that went silently dead (nothing closes it) is found
// out: a session opened under the call's context ends with it, the
// dead client is dropped and the next call dials anew; and keepalives
// notice the loss before any call does.
func TestSSHDeadConnectionIsReplaced(t *testing.T) {
	socket := serveUnix(t, pingHandler())
	s := sshtest.Start(t)
	var calls atomic.Int32
	s.Exec = dialStdioHook(t, socket, &calls)
	r := newRelay(t, net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	host, port, _ := net.SplitHostPort(r.addr())

	t.Run("a session that never opens drops the client, then a redial", func(t *testing.T) {
		d := newSSHDialer(sshConfig(t, s, true, map[string]any{"engine": "ssh://tester@" + host + ":" + port}))
		d.KeepaliveInterval = time.Hour // only the session's bound here
		d.SessionTimeout = 500 * time.Millisecond
		defer d.Close()
		e := newEngine(d.Dial)
		defer e.Close()
		if _, err := e.ping(ctx); err != nil {
			t.Fatal(err)
		}
		r.freeze()
		e.closeIdle()
		short, cancel := context.WithTimeout(ctx, time.Second)
		start := time.Now()
		_, err := e.ping(short)
		cancel()
		if err == nil || time.Since(start) > 5*time.Second {
			t.Fatalf("a call over the dead connection should fail at its deadline, got %v after %s", err, time.Since(start))
		}
		next, cancelNext := context.WithTimeout(ctx, 10*time.Second)
		defer cancelNext()
		if _, err := e.ping(next); err != nil {
			t.Fatalf("the next call should dial anew: %v", err)
		}
	})
	t.Run("keepalives drop a dead client", func(t *testing.T) {
		d := newSSHDialer(sshConfig(t, s, true, map[string]any{"engine": "ssh://tester@" + host + ":" + port}))
		d.KeepaliveInterval, d.KeepaliveTimeout = 100*time.Millisecond, 300*time.Millisecond
		defer d.Close()
		e := newEngine(d.Dial)
		defer e.Close()
		if _, err := e.ping(ctx); err != nil {
			t.Fatal(err)
		}
		r.freeze()
		e.closeIdle()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			d.mu.Lock()
			gone := d.client == nil
			d.mu.Unlock()
			if gone {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		d.mu.Lock()
		gone := d.client == nil
		d.mu.Unlock()
		if !gone {
			t.Fatal("the keepalive never dropped the dead client")
		}
		next, cancelNext := context.WithTimeout(ctx, 10*time.Second)
		defer cancelNext()
		if _, err := e.ping(next); err != nil {
			t.Fatalf("the next call should succeed at once on a fresh connection: %v", err)
		}
	})
}

// twoKeyServer is an SSH server with an RSA and an Ed25519 host key
// (a real docker host has several) that accepts any client.
func twoKeyServer(t *testing.T) (addr string, ed25519Line, rsaLine string) {
	t.Helper()
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	edSigner, _ := ssh.NewSignerFromKey(edPriv)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, _ := ssh.NewSignerFromKey(rsaKey)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(rsaSigner)
	cfg.AddHostKey(edSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					ch.Reject(ssh.Prohibited, "no channels")
				}
				conn.Close()
			}()
		}
	}()
	line := func(s ssh.Signer) string { return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) }
	return l.Addr().String(), line(edSigner), line(rsaSigner)
}

// The pinned key's type decides which host key the handshake asks
// for: the ed25519 line people copy from known_hosts must pin a host
// that also has an RSA key, which the library would otherwise prefer.
func TestSSHPinnedKeyTypeIsNegotiated(t *testing.T) {
	addr, edLine, rsaLine := twoKeyServer(t)
	key, _ := testKey(t)
	for _, pin := range []string{edLine, rsaLine} {
		cfg, err := parseConfig(map[string]any{"engine": "ssh://tester@" + addr, "ssh_private_key": key, "ssh_host_key": pin, "bind_address": "127.0.0.1", "agent_image": "x"})
		if err != nil {
			t.Fatal(err)
		}
		d := newSSHDialer(cfg)
		client, err := d.connect(ctx)
		if err != nil {
			t.Errorf("pinning %s: %v", strings.Fields(pin)[0], err)
			continue
		}
		client.Close()
		d.Close()
	}
}
