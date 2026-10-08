package targets_test

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

// sshConfigFixture is a stand-in for the mounted ~/.ssh: a config, the
// loopback server's client key and known_hosts.
type sshConfigFixture struct {
	dir    string
	server *sshtest.Server
}

func newSSHConfigFixture(t *testing.T, configBody func(f *sshConfigFixture) string) *sshConfigFixture {
	t.Helper()
	f := &sshConfigFixture{dir: shortTempDir(t), server: sshtest.Start(t)}
	key, err := os.ReadFile(f.server.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"id_ed25519":  string(key),
		"known_hosts": f.server.KnownHostsLine() + "\n",
		"config":      configBody(f),
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restore := targets.SetSSHConfigForTest(filepath.Join(f.dir, "config"), f.dir)
	t.Cleanup(restore)
	return f
}

func (f *sshConfigFixture) hostBlock(extra string) string {
	return "Host wyzer\n  HostName " + f.server.Host + "\n  Port " + strconv.Itoa(f.server.Port) +
		"\n  IdentityFile " + filepath.Join(f.dir, "id_ed25519") + "\n  UserKnownHostsFile " + filepath.Join(f.dir, "known_hosts") + "\n" + extra
}

var configTarget = &registry.Target{ID: "t1", Name: "wyzer", Kind: registry.TargetKindRemote, Host: "wyzer", User: "loomux-test"}

// LOOM-138: what the mounted config does for a config-mode target —
// its alias resolved, its key and host key found — becomes a managed
// target that connects the same way, with no config.
func TestResolveSSHConfig_MigratesAWorkingTarget(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 0 {
		t.Fatalf("problems: %v", plan.Problems)
	}
	if plan.Host != f.server.Host || plan.Port != f.server.Port || plan.User != "loomux-test" || plan.SSHProxy != registry.SSHProxyNone {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Key == nil || plan.Key.Origin != registry.SSHKeyOriginImported || plan.KeyFile != filepath.Join(f.dir, "id_ed25519") ||
		!strings.HasPrefix(plan.Key.PublicKey, "ssh-ed25519 ") || len(plan.Key.PrivateKey) == 0 {
		t.Fatalf("key = %+v (file %s)", plan.Key, plan.KeyFile)
	}
	if _, err := ssh.ParsePrivateKey(plan.Key.PrivateKey); err != nil {
		t.Errorf("imported private key doesn't parse: %v", err)
	}
	if !strings.HasPrefix(plan.HostKeys, "[127.0.0.1]:"+strconv.Itoa(f.server.Port)+" ") || !strings.HasSuffix(plan.HostKeys, f.server.HostKey) {
		t.Errorf("host keys = %q", plan.HostKeys)
	}

	// The plan, applied, connects: no config, the imported key, the pin.
	s := setupManaged(t, "")
	plan.Key.ID = "imported-" + strconv.Itoa(f.server.Port)
	m := *targets.CurrentManaged()
	m.Keys = func(context.Context, string) (*registry.SSHKey, error) { return plan.Key, nil }
	if err := targets.SetManagedSSH(&m); err != nil {
		t.Fatal(err)
	}
	_ = s
	managed := &registry.Target{ID: "t1-managed", Name: "wyzer", Kind: registry.TargetKindRemote, Host: plan.Host, SSHPort: plan.Port,
		User: plan.User, SSHKeyRef: plan.Key.ID, SSHProxy: plan.SSHProxy, HostKeys: plan.HostKeys}
	e := newManagedExecutor(t, managed)
	if out, err := e.RunOnce(context.Background(), "echo migrated"); err != nil || !strings.Contains(out, "migrated") {
		t.Fatalf("migrated target RunOnce = %q, %v", out, err)
	}
}

// An existing pin wins over known_hosts.
func TestResolveSSHConfig_KeepsThePin(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	if err := os.WriteFile(filepath.Join(f.dir, "known_hosts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pinned := *configTarget
	pinned.HostKeys = f.server.KnownHostsLine()
	plan, err := targets.ResolveSSHConfig(context.Background(), &pinned, "")
	if err != nil || len(plan.Problems) != 0 || !strings.HasSuffix(plan.HostKeys, f.server.HostKey) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

// A HostKeyAlias'd known_hosts entry is re-pinned under the real address
// a managed target is checked against.
func TestResolveSSHConfig_HostKeyAlias(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("  HostKeyAlias wyzer-alias\n") })
	if err := os.WriteFile(filepath.Join(f.dir, "known_hosts"), []byte("wyzer-alias "+f.server.HostKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil || len(plan.Problems) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if !strings.HasPrefix(plan.HostKeys, "[127.0.0.1]:") {
		t.Errorf("host keys = %q, want them under the real address", plan.HostKeys)
	}
}

// What doesn't map onto a managed target is reported, not guessed at.
func TestResolveSSHConfig_Problems(t *testing.T) {
	for name, tc := range map[string]struct {
		extra   string
		prepare func(f *sshConfigFixture)
		want    string
	}{
		"proxy jump":         {extra: "  ProxyJump bastion\n", want: "ProxyJump"},
		"unknown proxy":      {extra: "  ProxyCommand ssh -W %h:%p bastion\n", want: "ProxyCommand"},
		"other SOCKS5 proxy": {extra: "  ProxyCommand nc -X 5 -x 10.9.9.9:1080 %h %p\n", want: "LOOMUX_SSH_PROXY"},
		"no host key": {prepare: func(f *sshConfigFixture) {
			os.WriteFile(filepath.Join(f.dir, "known_hosts"), nil, 0o600)
		}, want: "host key"},
		"key outside ~/.ssh": {prepare: func(f *sshConfigFixture) {
			other := filepath.Join(t.TempDir(), "id_ed25519")
			data, _ := os.ReadFile(filepath.Join(f.dir, "id_ed25519"))
			os.WriteFile(other, data, 0o600)
			body := strings.Replace(f.hostBlock(""), filepath.Join(f.dir, "id_ed25519"), other, 1)
			os.WriteFile(filepath.Join(f.dir, "config"), []byte(body), 0o600)
		}, want: "key"},
		"passphrase-protected key": {prepare: func(f *sshConfigFixture) {
			key := filepath.Join(f.dir, "id_ed25519")
			os.Remove(key)
			if out, err := osexec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "secret", "-f", key).CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen: %v %s", err, out)
			}
		}, want: "passphrase"},
		"no key": {prepare: func(f *sshConfigFixture) {
			os.Remove(filepath.Join(f.dir, "id_ed25519"))
		}, want: "key"},
		"alias-only host name": {prepare: func(f *sshConfigFixture) {
			body := strings.Replace(f.hostBlock(""), "HostName "+f.server.Host, "HostName under_score", 1)
			os.WriteFile(filepath.Join(f.dir, "config"), []byte(body), 0o600)
		}, want: "host"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock(tc.extra) })
			if tc.prepare != nil {
				tc.prepare(f)
			}
			setupManaged(t, "127.0.0.1:1055") // the server's proxy
			plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(plan.Problems, "; "), tc.want) {
				t.Errorf("problems = %v, want one about %q", plan.Problems, tc.want)
			}
		})
	}
}

// The server's own SOCKS5 proxy in the config maps to ssh_proxy default.
func TestResolveSSHConfig_KnownProxy(t *testing.T) {
	for _, pc := range []string{"nc -X 5 -x 127.0.0.1:1055 %h %p", "/usr/bin/nc -X 5 -x 127.0.0.1:1055 %h %p", "socat - SOCKS5-CONNECT:127.0.0.1:1055:%h:%p"} {
		newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("  ProxyCommand " + pc + "\n") })
		setupManaged(t, "127.0.0.1:1055")
		plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
		if err != nil || len(plan.Problems) != 0 || plan.SSHProxy != "" {
			t.Errorf("%q: plan = %+v, %v", pc, plan, err)
		}
	}
}

func TestResolveSSHConfig_OnlyConfigModeRemotes(t *testing.T) {
	newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	for _, tgt := range []*registry.Target{
		{ID: "l", Name: "l", Kind: registry.TargetKindLocal},
		{ID: "m", Name: "m", Kind: registry.TargetKindRemote, Host: "h", User: "u", SSHKeyRef: "k"},
	} {
		if _, err := targets.ResolveSSHConfig(context.Background(), tgt, ""); err == nil {
			t.Errorf("%+v accepted", tgt)
		}
	}
}

// Like ssh, migration moves past a key it can't use to the next one the
// config lists (#301 review): an old passphrase-protected id_rsa doesn't
// stop the id_ed25519 beside it.
func TestResolveSSHConfig_SkipsUnusableKeys(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string {
		return "Host wyzer\n  HostName " + f.server.Host + "\n  Port " + strconv.Itoa(f.server.Port) +
			"\n  IdentityFile " + filepath.Join(f.dir, "id_rsa") + "\n  IdentityFile " + filepath.Join(f.dir, "id_ed25519") +
			"\n  UserKnownHostsFile " + filepath.Join(f.dir, "known_hosts") + "\n"
	})
	if out, err := osexec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "secret", "-f", filepath.Join(f.dir, "id_rsa")).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil || len(plan.Problems) != 0 || plan.KeyFile != filepath.Join(f.dir, "id_ed25519") {
		t.Fatalf("plan = %+v, %v; want id_ed25519 picked", plan, err)
	}
	// The caller can name the file outright.
	plan, err = targets.ResolveSSHConfig(context.Background(), configTarget, "id_rsa")
	if err != nil || !strings.Contains(strings.Join(plan.Problems, ";"), "passphrase") {
		t.Errorf("key_file id_rsa: %+v, %v", plan, err)
	}
	for _, bad := range []string{"../id_ed25519", "/etc/passwd", "sub/id", ".", ""} {
		if bad == "" {
			continue
		}
		plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, bad)
		if err != nil || len(plan.Problems) == 0 || plan.Key != nil {
			t.Errorf("key_file %q: %+v, %v; want refused", bad, plan, err)
		}
	}
}

// A symlink in ~/.ssh isn't followed, and an oversized file isn't read.
func TestResolveSSHConfig_KeyFileMustBeARegularFile(t *testing.T) {
	for name, prepare := range map[string]func(dir string){
		"symlink": func(dir string) {
			key := filepath.Join(dir, "id_ed25519")
			real := filepath.Join(dir, "real_key")
			os.Rename(key, real)
			os.Symlink(real, key)
		},
		"oversized": func(dir string) {
			os.WriteFile(filepath.Join(dir, "id_ed25519"), make([]byte, 64<<10), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
			prepare(f.dir)
			plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
			if err != nil || plan.Key != nil || len(plan.Problems) == 0 {
				t.Errorf("plan = %+v, %v; want the key refused", plan, err)
			}
		})
	}
}

// A key known_hosts revokes is never pinned (#301 review), however the
// host's own line lists it.
func TestResolveSSHConfig_RevokedHostKey(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	kh := "@revoked * " + f.server.HostKey + "\n" + f.server.KnownHostsLine() + "\n"
	if err := os.WriteFile(filepath.Join(f.dir, "known_hosts"), []byte(kh), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil || plan.HostKeys != "" || !strings.Contains(strings.Join(plan.Problems, ";"), "host key") {
		t.Errorf("plan = %+v, %v; want the revoked key not pinned", plan, err)
	}
}

// Hashed known_hosts entries are found and re-pinned in the clear.
func TestResolveSSHConfig_HashedKnownHosts(t *testing.T) {
	f := newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	if out, err := osexec.Command("ssh-keygen", "-H", "-f", filepath.Join(f.dir, "known_hosts")).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -H: %v %s", err, out)
	}
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil || len(plan.Problems) != 0 || !strings.HasPrefix(plan.HostKeys, "[127.0.0.1]:") {
		t.Errorf("plan = %+v, %v", plan, err)
	}
}

// No ProxyCommand means no proxy, whatever LOOMUX_SSH_PROXY is.
func TestResolveSSHConfig_NoProxyCommandWithServerProxy(t *testing.T) {
	newSSHConfigFixture(t, func(f *sshConfigFixture) string { return f.hostBlock("") })
	setupManaged(t, "127.0.0.1:1055")
	plan, err := targets.ResolveSSHConfig(context.Background(), configTarget, "")
	if err != nil || len(plan.Problems) != 0 || plan.SSHProxy != registry.SSHProxyNone {
		t.Errorf("plan = %+v, %v; want ssh_proxy none", plan, err)
	}
}
