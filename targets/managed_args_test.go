package targets

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

// recordingSSH is a fake ssh that writes its argv, one per line, to a
// file and succeeds.
func recordingSSH(t *testing.T) (argv func() []string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "argv")
	fakeSSH(t, `for a in "$@"; do printf '%s\n' "$a"; done > `+out+"\n")
	return func() []string {
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("fake ssh didn't run: %v", err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
}

func setupManagedArgs(t *testing.T, proxy string) *registry.Target {
	t.Helper()
	key, err := GenerateSSHKey("key-args", "args")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "lx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	pool := NewAgentPool(filepath.Join(dir, "agents"))
	t.Cleanup(func() { pool.Close() })
	if err := SetManagedSSH(&ManagedSSH{
		Keys:   func(context.Context, string) (*registry.SSHKey, error) { return key, nil },
		Agents: pool, KeysDir: filepath.Join(dir, "keys"), Proxy: proxy, Relay: "/usr/local/bin/loomuxd",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetManagedSSH(nil) })
	SetKnownHostsDir(filepath.Join(dir, "known_hosts.d"))
	return &registry.Target{ID: "t-args", Name: "args", Kind: registry.TargetKindRemote, Host: "wyzer.example", User: "orski",
		SSHPort: 2222, SSHKeyRef: key.ID, HostKeys: "[wyzer.example]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}
}

func optionValue(argv []string, name string) (string, bool) {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-o" && strings.HasPrefix(argv[i+1], name+"=") {
			return strings.TrimPrefix(argv[i+1], name+"="), true
		}
	}
	return "", false
}

// LOOM-138: the exact shape of a managed target's ssh command line. No
// ssh_config is read, only the managed key is offered, nothing is
// forwarded, the pin alone decides the host key, and the destination
// comes after "--".
func TestManagedArgs(t *testing.T) {
	tgt := setupManagedArgs(t, "127.0.0.1:1055")
	argv := recordingSSH(t)
	e, err := NewExecutor(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOnce(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	got := argv()
	if len(got) < 2 || got[0] != "-F" || got[1] != "/dev/null" {
		t.Errorf("argv doesn't start with -F /dev/null: %q", got)
	}
	for name, want := range map[string]string{
		"IdentitiesOnly": "yes", "PreferredAuthentications": "publickey", "PasswordAuthentication": "no",
		"KbdInteractiveAuthentication": "no", "ForwardAgent": "no", "ClearAllForwardings": "yes",
		"StrictHostKeyChecking": "yes", "GlobalKnownHostsFile": "/dev/null", "BatchMode": "yes",
		"ProxyCommand": "'/usr/local/bin/loomuxd' " + RelayFlag + " '127.0.0.1:1055' %h %p",
	} {
		if v, ok := optionValue(got, name); !ok || v != want {
			t.Errorf("-o %s = %q (set: %v), want %q", name, v, ok, want)
		}
	}
	if v, _ := optionValue(got, "IdentityAgent"); !strings.HasSuffix(v, ".sock") {
		t.Errorf("IdentityAgent = %q", v)
	}
	i := slices.Index(got, "-i")
	if i < 0 || !strings.HasSuffix(got[i+1], ".pub") {
		t.Fatalf("no -i <public key file>: %q", got)
	}
	if data, err := os.ReadFile(got[i+1]); err != nil || strings.Contains(string(data), "PRIVATE") {
		t.Errorf("-i file = %q, %v; want the public key only", data, err)
	}
	dd := slices.Index(got, "--")
	if dd < 0 || got[dd+1] != "orski@wyzer.example" || got[dd+2] != "/bin/sh" || len(got) != dd+3 {
		t.Errorf("destination isn't `-- orski@wyzer.example /bin/sh` at the end: %q", got[max(dd, 0):])
	}
	if p := slices.Index(got, "-p"); p < 0 || got[p+1] != "2222" {
		t.Errorf("port not passed: %q", got)
	}
}

func TestManagedArgs_NoProxy(t *testing.T) {
	tgt := setupManagedArgs(t, "127.0.0.1:1055")
	tgt.SSHProxy = registry.SSHProxyNone
	argv := recordingSSH(t)
	e, err := NewExecutor(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOnce(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if v, ok := optionValue(argv(), "ProxyCommand"); ok {
		t.Errorf("ProxyCommand = %q for ssh_proxy none", v)
	}
}

// A managed target never shares a ControlMaster with the same host
// reached through the SSH config (or with another key): a master
// authenticated one way must not carry the other's commands.
func TestManagedArgs_OwnControlPath(t *testing.T) {
	tgt := setupManagedArgs(t, "")
	argv := recordingSSH(t)
	run := func(tgt *registry.Target) string {
		e, err := NewExecutor(tgt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.RunOnce(context.Background(), "true"); err != nil {
			t.Fatal(err)
		}
		v, _ := optionValue(argv(), "ControlPath")
		return v
	}
	managed := run(tgt)
	cfg := *tgt
	cfg.SSHKeyRef = ""
	config := run(&cfg)
	other := *tgt
	other.SSHKeyRef = "key-other"
	if managed == config {
		t.Errorf("managed and config mode share ControlPath %s", managed)
	}
	if run(&other) == managed {
		t.Errorf("two keys share ControlPath %s", managed)
	}
	repinned := *tgt
	repinned.HostKeys = "[wyzer.example]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExampl"
	if run(&repinned) == managed {
		t.Errorf("a re-pinned target keeps the master verified against its old pin: %s", managed)
	}
	if len(managed) > maxSocketPath {
		t.Errorf("ControlPath %q is too long for a socket", managed)
	}
}

// The public key files' directory gets the same checks as the agents':
// a symlink planted there is refused (#294 review).
func TestManaged_KeysDirMustBeOurs(t *testing.T) {
	tgt := setupManagedArgs(t, "")
	m := currentManaged()
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "keys")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	mm := *m
	mm.KeysDir = link
	if err := SetManagedSSH(&mm); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExecutor(tgt); err == nil {
		t.Error("NewExecutor wrote the public key into a symlinked directory")
	}
}

// A long host name still makes a ControlPath ssh can create its
// temporary socket beside (it adds ~17 bytes).
func TestManaged_ControlPathBoundedForLongHosts(t *testing.T) {
	tgt := setupManagedArgs(t, "")
	tgt.Host = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + ".example"
	argv := recordingSSH(t)
	e, err := NewExecutor(tgt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOnce(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	cp, _ := optionValue(argv(), "ControlPath")
	if len(cp)+17 > maxSocketPath {
		t.Errorf("ControlPath %q (%d bytes) leaves ssh no room", cp, len(cp))
	}
}

// Once a key's agent runs, building another executor for the target
// doesn't load and decrypt the key again (#294 review).
func TestManaged_KeyLoadedOnce(t *testing.T) {
	tgt := setupManagedArgs(t, "")
	m := currentManaged()
	calls := 0
	mm := *m
	inner := m.Keys
	mm.Keys = func(ctx context.Context, id string) (*registry.SSHKey, error) {
		calls++
		return inner(ctx, id)
	}
	if err := SetManagedSSH(&mm); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := NewExecutor(tgt); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("key loaded %d times, want once", calls)
	}
	mm.Agents.Drop(tgt.SSHKeyRef)
	if _, err := NewExecutor(tgt); err != nil || calls != 2 {
		t.Errorf("after Drop: %v, %d loads; want the key loaded again", err, calls)
	}
}

func TestResetManagedSSH(t *testing.T) {
	setupManagedArgs(t, "")
	m := currentManaged()
	ResetManagedSSH(NewAgentPool(t.TempDir()))
	if currentManaged() != m {
		t.Error("another pool's reset cleared the config")
	}
	ResetManagedSSH(m.Agents)
	if currentManaged() != nil {
		t.Error("the owning pool's reset left the config in place")
	}
}
