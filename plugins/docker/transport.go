package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

// How the plugin reaches the engine (design §9): the local socket, or
// SSH to the docker host with the plugin's own key and the pinned host
// key, through a SOCKS5 proxy if configured, then docker system
// dial-stdio on the host, which proxies the session's stdio to the
// engine's socket (what the Docker CLI's ssh:// contexts do). One SSH
// connection per plugin instance, a session per HTTP connection,
// reconnected on loss.

// Errors the ssh dialer classifies, for plugin.check's problems.
var (
	errHostKeyUnpinned = errors.New("the docker host's key isn't pinned")
	errHostKeyMismatch = errors.New("the docker host's key doesn't match the pinned one")
	errAuth            = errors.New("the docker host refused the plugin's key")
)

// dialer connects to the engine and can be closed.
type dialer interface {
	Dial(ctx context.Context) (net.Conn, error)
	Close() error
}

// newDialer is the dialer for a configuration.
func newDialer(cfg Config) dialer {
	if cfg.Engine.Kind == engineUnix {
		return &unixDialer{path: cfg.Engine.Path}
	}
	return newSSHDialer(cfg)
}

// unixDialer is the engine's socket on this machine.
type unixDialer struct{ path string }

func unixDial(path string) dialFunc {
	return (&unixDialer{path: path}).Dial
}

func (d *unixDialer) Dial(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", d.path)
}

func (d *unixDialer) Close() error { return nil }

// sshDialer reaches the engine on the docker host over SSH.
type sshDialer struct {
	cfg Config
	// HandshakeTimeout bounds the TCP connect and the SSH handshake;
	// 20 s, under the host's 30 s call deadline.
	HandshakeTimeout time.Duration
	// KeepaliveInterval is how often the connection is probed; a probe
	// unanswered within KeepaliveTimeout drops the client, so a
	// connection dead without a close (a tailnet path change, a NAT
	// timeout) is replaced by the next call; 15 s and 10 s.
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	// SessionTimeout bounds opening a session (milliseconds on a live
	// connection): one that takes longer is on a dead connection, which
	// is dropped. net/http dials with a context detached from the
	// request's, so the call's own deadline doesn't reach here; 10 s.
	SessionTimeout time.Duration

	mu     sync.Mutex
	client *ssh.Client
}

func newSSHDialer(cfg Config) *sshDialer {
	return &sshDialer{cfg: cfg, HandshakeTimeout: 20 * time.Second, KeepaliveInterval: 15 * time.Second, KeepaliveTimeout: 10 * time.Second, SessionTimeout: 10 * time.Second}
}

func (d *sshDialer) addr() string {
	return net.JoinHostPort(d.cfg.Engine.Host, strconv.Itoa(d.cfg.Engine.Port))
}

// dialTCP connects to the docker host, through the SOCKS5 proxy when
// one is configured.
func (d *sshDialer) dialTCP(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, d.HandshakeTimeout)
	defer cancel()
	direct := &net.Dialer{}
	if d.cfg.Proxy == "" {
		return direct.DialContext(ctx, "tcp", d.addr())
	}
	p, err := proxy.SOCKS5("tcp", d.cfg.Proxy, nil, direct)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", d.cfg.Proxy, err)
	}
	conn, err := p.(proxy.ContextDialer).DialContext(ctx, "tcp", d.addr())
	if err != nil {
		return nil, fmt.Errorf("through proxy %s: %w", d.cfg.Proxy, err)
	}
	return conn, nil
}

// handshake runs the SSH handshake on a fresh TCP connection with
// callback deciding about the host key. Its errors are classified.
func (d *sshDialer) handshake(ctx context.Context, callback ssh.HostKeyCallback, algorithms []string) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	conn, err := d.dialTCP(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connecting to %s: %w", d.cfg.Engine, err)
	}
	_ = conn.SetDeadline(time.Now().Add(d.HandshakeTimeout))
	auth := []ssh.AuthMethod{}
	if d.cfg.signer != nil {
		auth = append(auth, ssh.PublicKeys(d.cfg.signer))
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, d.addr(), &ssh.ClientConfig{
		User:              d.cfg.Engine.User,
		Auth:              auth,
		HostKeyCallback:   callback,
		HostKeyAlgorithms: algorithms,
		Timeout:           d.HandshakeTimeout,
		ClientVersion:     "SSH-2.0-loomux-plugin-docker_" + Version,
	})
	if err != nil {
		conn.Close()
		return nil, nil, nil, classify(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return c, chans, reqs, nil
}

// classify maps the ssh library's errors to the plugin's.
func classify(err error) error {
	switch {
	case errors.Is(err, errHostKeyMismatch), errors.Is(err, errHostKeyUnpinned), errors.Is(err, errScanned):
		return err
	case strings.Contains(err.Error(), "unable to authenticate"):
		return fmt.Errorf("%w: %v", errAuth, err)
	}
	return err
}

// connect is the SSH client, made on first use and after a loss. No
// pin, no connection: an unpinned host is only ever scanned.
func (d *sshDialer) connect(ctx context.Context) (*ssh.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	if d.cfg.hostKey == nil {
		return nil, errHostKeyUnpinned
	}
	pinned := d.cfg.hostKey.Marshal()
	// The handshake asks for the pinned key's type: a host with several
	// host keys would otherwise present the library's favourite (RSA)
	// and the ed25519 line people copy from known_hosts would never
	// match.
	c, chans, reqs, err := d.handshake(ctx, func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if bytes.Equal(key.Marshal(), pinned) {
			return nil
		}
		return fmt.Errorf("%w: the host presented %s %s", errHostKeyMismatch, key.Type(), ssh.FingerprintSHA256(key))
	}, hostKeyAlgorithms(d.cfg.hostKey))
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	d.client = client
	go func() {
		_ = client.Wait()
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
	}()
	go d.keepalive(client)
	return client, nil
}

// hostKeyAlgorithms are the handshake's host key algorithms for a
// pinned key: the key's own type, with RSA's SHA-2 signatures first.
func hostKeyAlgorithms(pub ssh.PublicKey) []string {
	switch pub.Type() {
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{pub.Type()}
}

// keepalive probes the connection every KeepaliveInterval and closes
// the client when a probe isn't answered within KeepaliveTimeout: a
// connection dead without a close is dropped, and the next call dials
// anew instead of hanging on it.
func (d *sshDialer) keepalive(client *ssh.Client) {
	ticker := time.NewTicker(d.KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-clientDone(client):
			return
		}
		answered := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				client.Close()
				return
			}
		case <-time.After(d.KeepaliveTimeout):
			client.Close()
			return
		}
	}
}

// clientDone is closed when the client's connection ends.
func clientDone(client *ssh.Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()
	return done
}

// newSession opens a session within SessionTimeout (and ctx): a client
// whose connection is dead never answers, so the client is closed and
// dropped (the next call dials anew) and the call fails instead of
// hanging until the kernel gives up on the connection.
func (d *sshDialer) newSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, d.SessionTimeout)
	defer cancel()
	type result struct {
		sess *ssh.Session
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sess, err := client.NewSession()
		ch <- result{sess, err}
	}()
	select {
	case r := <-ch:
		return r.sess, r.err
	case <-ctx.Done():
		client.Close()
		go func() {
			if r := <-ch; r.sess != nil {
				r.sess.Close()
			}
		}()
		return nil, fmt.Errorf("ssh session to %s: the connection isn't answering (%w)", d.cfg.Engine, ctx.Err())
	}
}

// Dial opens a session running docker system dial-stdio on the host
// and returns it as a connection to the engine.
func (d *sshDialer) Dial(ctx context.Context) (net.Conn, error) {
	client, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := d.newSession(ctx, client)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// The connection may have died since: drop it and try once more.
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
		client.Close()
		if client, err = d.connect(ctx); err != nil {
			return nil, err
		}
		if sess, err = d.newSession(ctx, client); err != nil {
			return nil, fmt.Errorf("ssh session to %s: %w", d.cfg.Engine, err)
		}
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	sc := &sessionConn{sess: sess, stdin: stdin, stdout: stdout, stderr: &boundedBuffer{limit: 4 << 10}, done: make(chan struct{})}
	sess.Stderr = sc.stderr
	if err := sess.Start("docker system dial-stdio"); err != nil {
		sess.Close()
		return nil, fmt.Errorf("starting docker system dial-stdio on %s: %w", d.cfg.Engine, err)
	}
	go func() {
		sc.waitErr = sess.Wait()
		close(sc.done)
	}()
	return sc, nil
}

// errScanned is how a scan's host key callback ends the handshake:
// before any authentication.
var errScanned = errors.New("host key recorded")

// Scan learns the docker host's key without trusting it or
// authenticating to it: the handshake is refused once the key is seen.
func (d *sshDialer) Scan(ctx context.Context) (ssh.PublicKey, error) {
	var seen ssh.PublicKey
	_, _, _, err := d.handshake(ctx, func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		seen = key
		return errScanned
	}, nil)
	if seen != nil {
		return seen, nil
	}
	if err == nil {
		return nil, errors.New("the handshake ended without a host key")
	}
	return nil, err
}

func (d *sshDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		err := d.client.Close()
		d.client = nil
		return err
	}
	return nil
}

// sessionConn is a dial-stdio session as a net.Conn: writes go to the
// command's stdin, reads come from its stdout. An EOF with the command
// gone wrong (docker missing on the host, the user not in the docker
// group) is reported with the command's stderr instead.
type sessionConn struct {
	sess    *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	stderr  *boundedBuffer
	done    chan struct{}
	waitErr error

	closeOnce sync.Once
}

func (c *sessionConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	return n, c.explain(err)
}

func (c *sessionConn) Write(p []byte) (int, error) {
	n, err := c.stdin.Write(p)
	return n, c.explain(err)
}

// explain turns an EOF or a closed channel, when the command itself
// went wrong, into an error naming what its stderr said.
func (c *sessionConn) explain(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		return err
	}
	msg := oneLine(c.stderr.String())
	if msg == "" && c.waitErr != nil {
		msg = c.waitErr.Error()
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("docker system dial-stdio on the docker host failed: %s", msg)
}

// Close closes the session (the channel, which ends the remote command),
// never stdin on its own: net/http closes a connection from one
// goroutine while another may still be writing, and the ssh channel's
// CloseWrite isn't safe against a concurrent Write where Close is.
func (c *sessionConn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.sess.Close() })
	return err
}

func (c *sessionConn) LocalAddr() net.Addr                { return sessionAddr("local") }
func (c *sessionConn) RemoteAddr() net.Addr               { return sessionAddr("docker") }
func (c *sessionConn) SetDeadline(t time.Time) error      { return nil }
func (c *sessionConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *sessionConn) SetWriteDeadline(t time.Time) error { return nil }

type sessionAddr string

func (a sessionAddr) Network() string { return "ssh" }
func (a sessionAddr) String() string  { return string(a) }

// boundedBuffer keeps the first limit bytes written to it.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
