package targets_test

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

func TestGenerateSSHKey(t *testing.T) {
	k, err := targets.GenerateSSHKey("key-1", "wyzer")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != "key-1" || k.Name != "wyzer" || k.Type != "ssh-ed25519" || k.Origin != registry.SSHKeyOriginGenerated {
		t.Fatalf("key = %+v", k)
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
	if err != nil || len(rest) != 0 {
		t.Fatalf("public key %q: %v", k.PublicKey, err)
	}
	if comment != "loomux-wyzer" || strings.Contains(k.PublicKey, "\n") {
		t.Errorf("public key line = %q, want one line with comment loomux-wyzer", k.PublicKey)
	}
	if k.Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Errorf("fingerprint = %q, want %q", k.Fingerprint, ssh.FingerprintSHA256(pub))
	}
	signer, err := ssh.ParsePrivateKey(k.PrivateKey)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Error("private and public key don't match")
	}
	other, _ := targets.GenerateSSHKey("key-2", "wyzer")
	if other.Fingerprint == k.Fingerprint {
		t.Error("two generated keys are the same")
	}
}

func TestGenerateSSHKey_RefusesBadNames(t *testing.T) {
	for _, name := range []string{"", "-x", "a b", "a\nssh-ed25519 AAAA evil", "a/b", strings.Repeat("a", 65), "é"} {
		if _, err := targets.GenerateSSHKey("id", name); err == nil {
			t.Errorf("GenerateSSHKey(%q) accepted", name)
		}
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	// unix socket paths are limited to ~108 bytes; t.TempDir can be long.
	dir, err := os.MkdirTemp("", "lx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// LOOM-138: each managed key is served by its own agent, which signs but
// can't be added to, emptied or locked, from a directory only loomuxd's
// user can enter.
func TestAgentPool_ServesOneKeyReadOnly(t *testing.T) {
	k, err := targets.GenerateSSHKey("key-1", "wyzer")
	if err != nil {
		t.Fatal(err)
	}
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), "agents"))
	defer pool.Close()
	sock, err := pool.Socket(k)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := pool.Socket(k); err != nil || again != sock {
		t.Errorf("Socket again = %q, %v; want the same socket", again, err)
	}
	if fi, err := os.Stat(filepath.Dir(sock)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agent.NewClient(conn)
	keys, err := client.List()
	if err != nil || len(keys) != 1 || ssh.FingerprintSHA256(keys[0]) != k.Fingerprint {
		t.Fatalf("List = %v, %v", keys, err)
	}
	sig, err := client.Sign(keys[0], []byte("data"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := keys[0].Verify([]byte("data"), sig); err != nil {
		t.Errorf("signature doesn't verify: %v", err)
	}

	other, _ := targets.GenerateSSHKey("key-2", "other")
	otherKey, _ := ssh.ParseRawPrivateKey(other.PrivateKey)
	if err := client.Add(agent.AddedKey{PrivateKey: otherKey}); err == nil {
		t.Error("Add accepted")
	}
	if err := client.RemoveAll(); err == nil {
		t.Error("RemoveAll accepted")
	}
	if err := client.Lock([]byte("x")); err == nil {
		t.Error("Lock accepted")
	}
	if keys, _ := client.List(); len(keys) != 1 {
		t.Errorf("after refused writes the agent holds %d keys", len(keys))
	}
}

func TestAgentPool_DropStopsServing(t *testing.T) {
	k, _ := targets.GenerateSSHKey("key-1", "wyzer")
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), "agents"))
	defer pool.Close()
	sock, err := pool.Socket(k)
	if err != nil {
		t.Fatal(err)
	}
	pool.Drop("key-1")
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still there after Drop: %v", err)
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Error("agent still answers after Drop")
	}
}

func TestAgentPool_NeedsThePrivateKey(t *testing.T) {
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), "agents"))
	defer pool.Close()
	if _, err := pool.Socket(&registry.SSHKey{ID: "k", Name: "n"}); err == nil {
		t.Error("Socket without a private key accepted")
	}
	if _, err := pool.Socket(&registry.SSHKey{ID: "../escape", Name: "n", PrivateKey: []byte("x")}); err == nil {
		t.Error("Socket with a path-like id accepted")
	}
}

// Review of #280: a Drop racing a Socket for the same key must never
// leave the pool handing out a socket that's gone.
func TestAgentPool_DropAndSocketRace(t *testing.T) {
	k, _ := targets.GenerateSSHKey("key-1", "wyzer")
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), "agents"))
	defer pool.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			pool.Drop("key-1")
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := pool.Socket(k); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	sock, err := pool.Socket(k)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the pool's socket doesn't answer: %v", err)
	}
	conn.Close()
}

func TestAgentPool_SocketIsPrivate(t *testing.T) {
	k, _ := targets.GenerateSSHKey("key-1", "wyzer")
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), "agents"))
	defer pool.Close()
	sock, err := pool.Socket(k)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

// The agent directory must be a real directory of loomuxd's own: a
// symlink planted where it should be is refused, and an existing one is
// made private.
func TestAgentPool_RefusesASymlinkedDir(t *testing.T) {
	base := shortTempDir(t)
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "agents")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	k, _ := targets.GenerateSSHKey("key-1", "wyzer")
	pool := targets.NewAgentPool(link)
	defer pool.Close()
	if _, err := pool.Socket(k); err == nil {
		t.Error("Socket served from a symlinked directory")
	}

	existing := filepath.Join(base, "open")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	pool2 := targets.NewAgentPool(existing)
	defer pool2.Close()
	if _, err := pool2.Socket(k); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(existing); fi.Mode().Perm() != 0o700 {
		t.Errorf("existing dir left at %v, want 0700", fi.Mode().Perm())
	}
}

func TestAgentPool_SocketPathTooLong(t *testing.T) {
	k, _ := targets.GenerateSSHKey("key-1", "wyzer")
	pool := targets.NewAgentPool(filepath.Join(shortTempDir(t), strings.Repeat("d", 120)))
	defer pool.Close()
	if _, err := pool.Socket(k); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("Socket with an over-long path: %v, want a clear 'too long' error", err)
	}
}
