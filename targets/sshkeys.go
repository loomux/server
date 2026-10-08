package targets

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/Loomux/server/registry"
)

// SSH keys Loomux manages (LOOM-138, docs/design/target-onboarding.md):
// generated here, stored encrypted by the registry, and handed to ssh
// only through an in-process agent, so a private key is never written to
// disk in the clear.

// sshKeyName is what a managed key's name may be: it ends up in the
// public key's comment, on the line a person appends to authorized_keys.
var sshKeyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidSSHKeyName reports whether name can name a managed SSH key.
func ValidSSHKeyName(name string) bool { return sshKeyName.MatchString(name) }

// sshKeyID is what a key id may be: it names the key's agent socket.
var sshKeyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// GenerateSSHKey makes a new ed25519 key pair called name, commented
// "loomux-<name>" so a person can tell it apart in authorized_keys.
func GenerateSSHKey(id, name string) (*registry.SSHKey, error) {
	if !ValidSSHKeyName(name) {
		return nil, fmt.Errorf("targets: ssh key name %q: letters, digits, '.', '_' and '-' only, at most 64", name)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("targets: generate ssh key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("targets: generate ssh key: %w", err)
	}
	comment := "loomux-" + name
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, fmt.Errorf("targets: generate ssh key: %w", err)
	}
	return &registry.SSHKey{
		ID:          id,
		Name:        name,
		Type:        sshPub.Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment,
		Fingerprint: ssh.FingerprintSHA256(sshPub),
		Origin:      registry.SSHKeyOriginGenerated,
		PrivateKey:  pem.EncodeToMemory(block),
	}, nil
}

// AgentPool serves each managed key from an agent of its own, on a unix
// socket in a 0700 directory: ssh is pointed at one with IdentityAgent.
// An agent only lists and signs with its one key; adding, removing and
// locking are refused.
//
// Anything running as loomuxd's user can reach the sockets — agents on a
// local target included — but such a process can already read the
// database and the master key, so the sockets add nothing to what it
// could do.
type AgentPool struct {
	dir string

	mu     sync.Mutex
	agents map[string]*servedAgent
	swept  bool
}

type servedAgent struct {
	socket   string
	listener net.Listener

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// maxSocketPath is the longest unix socket path every platform takes
// (sun_path: 108 bytes on Linux, 104 on macOS and the BSDs).
const maxSocketPath = 103

// ensureDir makes p.dir a private directory of loomuxd's own: created
// 0700 if missing, refused if it is a symlink, not a directory or owned
// by another user (who could swap a socket in it for an agent of their
// own), made 0700 otherwise. The first time, sockets an earlier process
// left there are removed: the dir belongs to one loomuxd process.
func (p *AgentPool) ensureDir() error {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return fmt.Errorf("targets: agent dir: %w", err)
	}
	fi, err := os.Lstat(p.dir)
	if err != nil {
		return fmt.Errorf("targets: agent dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("targets: agent dir %s is not a directory", p.dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("targets: agent dir %s belongs to another user", p.dir)
	}
	if err := os.Chmod(p.dir, 0o700); err != nil {
		return fmt.Errorf("targets: agent dir: %w", err)
	}
	if !p.swept {
		old, _ := filepath.Glob(filepath.Join(p.dir, "*.sock"))
		for _, f := range old {
			_ = os.Remove(f)
		}
		p.swept = true
	}
	return nil
}

// NewAgentPool serves agents from sockets in dir, which it creates.
func NewAgentPool(dir string) *AgentPool {
	return &AgentPool{dir: dir, agents: map[string]*servedAgent{}}
}

// Socket returns the socket of k's agent, starting it if it isn't
// running. k must carry its private key (registry GetSSHKey).
func (p *AgentPool) Socket(k *registry.SSHKey) (string, error) {
	if !sshKeyID.MatchString(k.ID) {
		return "", fmt.Errorf("targets: ssh key id %q can't name a socket", k.ID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.agents[k.ID]; ok {
		return a.socket, nil
	}
	if len(k.PrivateKey) == 0 {
		return "", errors.New("targets: ssh key has no private key")
	}
	raw, err := ssh.ParseRawPrivateKey(k.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("targets: ssh key %q: %w", k.Name, err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: raw, Comment: "loomux-" + k.Name}); err != nil {
		return "", fmt.Errorf("targets: ssh key %q: %w", k.Name, err)
	}
	if err := p.ensureDir(); err != nil {
		return "", err
	}
	// A name of its own per agent, so a Drop of an earlier one racing
	// this can never unlink this socket.
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("targets: agent socket: %w", err)
	}
	socket := filepath.Join(p.dir, k.ID+"-"+hex.EncodeToString(suffix)+".sock")
	if len(socket) > maxSocketPath {
		return "", fmt.Errorf("targets: agent socket path %q is too long for a unix socket (at most %d bytes)", socket, maxSocketPath)
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return "", fmt.Errorf("targets: agent socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		l.Close()
		return "", fmt.Errorf("targets: agent socket: %w", err)
	}
	a := &servedAgent{socket: socket, listener: l, conns: map[net.Conn]struct{}{}}
	go a.serve(readOnlyAgent{keyring.(agent.ExtendedAgent)})
	p.agents[k.ID] = a
	return socket, nil
}

// Drop stops serving key id (deleted, or rotated away from).
func (p *AgentPool) Drop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.agents[id]; ok {
		delete(p.agents, id)
		a.close()
	}
}

// Close stops every agent.
func (p *AgentPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.agents {
		a.close()
	}
	p.agents = map[string]*servedAgent{}
	return nil
}

func (a *servedAgent) serve(ag agent.Agent) {
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			conn.Close()
			return
		}
		a.conns[conn] = struct{}{}
		a.mu.Unlock()
		go func() {
			_ = agent.ServeAgent(ag, conn)
			conn.Close()
			a.mu.Lock()
			delete(a.conns, conn)
			a.mu.Unlock()
		}()
	}
}

func (a *servedAgent) close() {
	a.listener.Close() // removes the socket file too
	_ = os.Remove(a.socket)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	for c := range a.conns {
		c.Close()
	}
}

// errReadOnlyAgent is every change refused by a managed key's agent.
var errReadOnlyAgent = errors.New("loomux agent is read-only")

// readOnlyAgent lists and signs with its keyring and refuses the rest.
type readOnlyAgent struct{ kr agent.ExtendedAgent }

func (r readOnlyAgent) List() ([]*agent.Key, error) { return r.kr.List() }
func (r readOnlyAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return r.kr.Sign(key, data)
}
func (r readOnlyAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	return r.kr.SignWithFlags(key, data, flags)
}
func (readOnlyAgent) Add(agent.AddedKey) error       { return errReadOnlyAgent }
func (readOnlyAgent) Remove(ssh.PublicKey) error     { return errReadOnlyAgent }
func (readOnlyAgent) RemoveAll() error               { return errReadOnlyAgent }
func (readOnlyAgent) Lock([]byte) error              { return errReadOnlyAgent }
func (readOnlyAgent) Unlock([]byte) error            { return errReadOnlyAgent }
func (readOnlyAgent) Signers() ([]ssh.Signer, error) { return nil, errReadOnlyAgent }
func (readOnlyAgent) Extension(string, []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}
