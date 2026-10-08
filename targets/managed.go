package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/Loomux/server/registry"
)

// Managed targets (LOOM-138, docs/design/target-onboarding.md): reached
// with a Loomux-managed key, served by its in-process agent, and no
// ssh_config at all (-F /dev/null). Everything ssh needs is on its
// command line, built from validated target fields and the server's own
// configuration: the key (an agent socket and its public key file), the
// pinned host key (required), and the SOCKS5 proxy, relayed by loomuxd
// itself as the ProxyCommand.

// RelayFlag is the loomuxd flag ssh's ProxyCommand runs it with:
// loomuxd -ssh-proxy-connect PROXY HOST PORT.
const RelayFlag = "-ssh-proxy-connect"

// ManagedSSH is how managed targets are reached. Set once at startup
// with SetManagedSSH.
type ManagedSSH struct {
	// Keys loads a key with its private key (registry GetSSHKey).
	Keys func(ctx context.Context, id string) (*registry.SSHKey, error)
	// Agents serves the keys to ssh.
	Agents *AgentPool
	// KeysDir holds the keys' public key files, which tell ssh which of
	// an agent's keys to offer (IdentitiesOnly).
	KeysDir string
	// Proxy is the SOCKS5 proxy, host:port, that targets reach through
	// unless they opt out (LOOMUX_SSH_PROXY); "" for none.
	Proxy string
	// Relay is the absolute path of the loomuxd binary ssh runs as the
	// ProxyCommand.
	Relay string
}

var (
	managedMu sync.RWMutex
	managed   *ManagedSSH
)

// relayPath and proxyAddr are what can go into the ProxyCommand, which
// ssh hands to a shell after expanding %-tokens: nothing a shell or ssh
// would read as anything but itself.
var (
	relayPath     = regexp.MustCompile(`^/[A-Za-z0-9/._+-]*$`)
	proxyHostName = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)
)

// validHostPort reports whether hostport is host:port with a plain host
// (a name or an address) and a port number.
func validHostPort(host, port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536 && strconv.Itoa(n) == port &&
		proxyHostName.MatchString(host) && !strings.HasPrefix(host, "-")
}

// SetManagedSSH configures how managed targets are reached; nil turns
// them off (they then fail to connect, saying so).
func SetManagedSSH(m *ManagedSSH) error {
	if m != nil {
		switch {
		case m.Keys == nil || m.Agents == nil || m.KeysDir == "":
			return errors.New("targets: managed SSH needs a key loader, an agent pool and a keys directory")
		case !relayPath.MatchString(m.Relay):
			return fmt.Errorf("targets: relay %q must be an absolute path of letters, digits and /._+-", m.Relay)
		}
		if m.Proxy != "" {
			host, port, err := net.SplitHostPort(m.Proxy)
			if err != nil || !validHostPort(host, port) {
				return fmt.Errorf("targets: SSH proxy %q must be host:port", m.Proxy)
			}
		}
	}
	managedMu.Lock()
	defer managedMu.Unlock()
	managed = m
	return nil
}

// ResetManagedSSH turns managed SSH off if it is the configuration using
// agents: their owner is closing them.
func ResetManagedSSH(agents *AgentPool) {
	managedMu.Lock()
	defer managedMu.Unlock()
	if managed != nil && managed.Agents == agents {
		managed = nil
	}
}

func currentManaged() *ManagedSSH {
	managedMu.RLock()
	defer managedMu.RUnlock()
	return managed
}

// managedKeyTimeout bounds loading and decrypting a key.
const managedKeyTimeout = 10 * time.Second

// managedOptions are a managed target's options on top of its port and
// pin (remoteOptions).
func managedOptions(t *registry.Target) ([]RemoteOption, error) {
	m := currentManaged()
	if m == nil {
		return nil, errors.New("targets: this target uses a Loomux SSH key, but managed SSH is not configured on this server (is LOOMUX_MASTER_KEY set?)")
	}
	if strings.TrimSpace(t.HostKeys) == "" {
		return nil, &UnreachableError{Host: t.Host, Failure: SSHHostKeyUnknown, Detail: "no host key is pinned for this target", Managed: true}
	}
	socket, pub, err := managedIdentity(m, t.SSHKeyRef)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-o", "IdentityAgent=" + socket,
		"-o", "IdentitiesOnly=yes",
		"-i", pub,
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
	}
	args = append(args, managedProxyArgs(m, t)...)
	return []RemoteOption{func(e *RemoteExecutor) {
		e.sshConfig = "/dev/null"
		e.extraArgs = append(e.extraArgs, args...)
		e.controlTag = managedControlTag(m, t)
	}}, nil
}

// managedIdentity is key id's agent socket and public key file. The key
// is only loaded and decrypted when its agent isn't running yet: an
// executor is built for every operation's caller, often (#294 review).
func managedIdentity(m *ManagedSSH, id string) (socket, pub string, err error) {
	if socket, ok := m.Agents.Running(id); ok && sshKeyID.MatchString(id) {
		pub = filepath.Join(m.KeysDir, id+".pub")
		if _, err := os.Stat(pub); err == nil {
			return socket, pub, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedKeyTimeout)
	defer cancel()
	key, err := m.Keys(ctx, id)
	if err != nil {
		return "", "", fmt.Errorf("targets: load the target's SSH key: %w", err)
	}
	if socket, err = m.Agents.Socket(key); err != nil {
		return "", "", err
	}
	if pub, err = writePublicKey(m.KeysDir, key); err != nil {
		return "", "", err
	}
	return socket, pub, nil
}

// managedProxyArgs is the ProxyCommand for t, if it goes through the
// server's proxy. The relay and proxy were vetted by SetManagedSSH, and
// %h is t's host, which Target.Validate holds to a host name or address.
func managedProxyArgs(m *ManagedSSH, t *registry.Target) []string {
	if m.Proxy == "" || t.SSHProxy == registry.SSHProxyNone {
		return nil
	}
	// No "exec" of our own: ssh runs the command as "exec <command>"
	// under $SHELL already, and only zsh takes "exec exec" (#294 review).
	return []string{"-o", "ProxyCommand='" + m.Relay + "' " + RelayFlag + " '" + m.Proxy + "' %h %p"}
}

// managedControlTag sets a managed target's ControlMaster apart from any
// other way of reaching the same user@host:port — through the SSH
// config, with another key or proxy, or under an earlier pin — so a
// master authenticated or verified one way never carries commands meant
// for another.
func managedControlTag(m *ManagedSSH, t *registry.Target) string {
	proxy := ""
	if t.SSHProxy != registry.SSHProxyNone {
		proxy = m.Proxy
	}
	sum := sha256.Sum256([]byte(t.SSHKeyRef + "\x00" + proxy + "\x00" + strings.TrimSpace(t.HostKeys)))
	return hex.EncodeToString(sum[:5])
}

// writePublicKey writes k's public key to dir/<id>.pub, if it isn't
// there already, and returns its path. It is no secret; ssh only uses it
// to pick the agent's key.
func writePublicKey(dir string, k *registry.SSHKey) (string, error) {
	if !sshKeyID.MatchString(k.ID) {
		return "", fmt.Errorf("targets: ssh key id %q can't name a file", k.ID)
	}
	if err := ensurePrivateDir(dir, "keys dir"); err != nil {
		return "", err
	}
	path := filepath.Join(dir, k.ID+".pub")
	want := []byte(k.PublicKey + "\n")
	if have, err := os.ReadFile(path); err == nil && string(have) == string(want) {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, k.ID+".pub.*")
	if err != nil {
		return "", fmt.Errorf("targets: write public key: %w", err)
	}
	_, werr := tmp.Write(want)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("targets: write public key: %w", errors.Join(werr, cerr))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("targets: write public key: %w", err)
	}
	return path, nil
}

// relayDialTimeout bounds the relay's connection through the proxy.
const relayDialTimeout = 10 * time.Second

// RelaySOCKS5 connects to host:port through the SOCKS5 proxy at
// proxyAddr and relays in and out over it: loomuxd as ssh's ProxyCommand
// (RelayFlag), so a managed target needs neither an ssh_config nor nc.
func RelaySOCKS5(proxyAddr, host, port string, in io.Reader, out io.Writer) error {
	if !validHostPort(host, port) {
		return fmt.Errorf("loomuxd relay: refusing destination %q port %q", host, port)
	}
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, &net.Dialer{Timeout: relayDialTimeout})
	if err != nil {
		return fmt.Errorf("loomuxd relay: %w", err)
	}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("loomuxd relay: connect to %s through %s: %w", net.JoinHostPort(host, port), proxyAddr, err)
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, err = io.Copy(out, conn)
	return err
}
