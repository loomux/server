package targets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// LOOM-166: the ControlMaster socket dir sits at a predictable path in
// the shared temp dir. Before ssh runs, it and its parent must be
// loomuxd's own private directories: not symlinks, not someone else's,
// 0700.
func TestRemoteExecutor_ControlDirIsPrivate(t *testing.T) {
	newExec := func(t *testing.T) (*RemoteExecutor, string, string) {
		t.Helper()
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		e := NewRemoteExecutor("127.0.0.1", "nobody", WithPort(1))
		return e, filepath.Join(tmp, "loomux"), filepath.Dir(e.controlPath)
	}
	refused := func(t *testing.T, e *RemoteExecutor, want string) {
		t.Helper()
		_, _, _, err := e.sshExec(context.Background(), "true")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("sshExec = %v, want it refused (%s) before ssh runs", err, want)
		}
	}

	t.Run("created private", func(t *testing.T) {
		e, parent, dir := newExec(t)
		if err := ensureControlDir(filepath.Dir(e.controlPath)); err != nil {
			t.Fatalf("ensureControlDir: %v", err)
		}
		for _, d := range []string{parent, dir} {
			if fi, err := os.Lstat(d); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Fatalf("%s: %v, %v; want a 0700 directory", d, fi, err)
			}
		}
	})
	t.Run("loose permissions tightened", func(t *testing.T) {
		e, parent, dir := newExec(t)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{parent, dir} {
			if err := os.Chmod(d, 0o777); err != nil {
				t.Fatal(err)
			}
		}
		if err := ensureControlDir(filepath.Dir(e.controlPath)); err != nil {
			t.Fatalf("ensureControlDir: %v", err)
		}
		for _, d := range []string{parent, dir} {
			if fi, _ := os.Lstat(d); fi.Mode().Perm() != 0o700 {
				t.Fatalf("%s left %v, want 0700", d, fi.Mode().Perm())
			}
		}
	})
	t.Run("symlinked socket dir refused", func(t *testing.T) {
		e, parent, dir := newExec(t)
		elsewhere := t.TempDir()
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
		refused(t, e, "is not a directory")
	})
	t.Run("symlinked parent refused", func(t *testing.T) {
		e, parent, _ := newExec(t)
		if err := os.Symlink(t.TempDir(), parent); err != nil {
			t.Fatal(err)
		}
		refused(t, e, "is not a directory")
	})
	t.Run("another user's dir refused", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to make a directory another user owns")
		}
		for _, which := range []string{"parent", "socket dir"} {
			e, parent, dir := newExec(t)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			owned := dir
			if which == "parent" {
				owned = parent
			}
			if err := os.Chown(owned, 65534, 65534); err != nil {
				t.Fatal(err)
			}
			refused(t, e, "belongs to another user")
		}
	})
}

// LOOM-188: ssh expands %-tokens in ControlPath. A config-mode target's
// socket name holds its host, and a link-local host's zone id ("%eth0")
// must reach ssh as itself, not as an unknown token.
func TestRemoteExecutor_ControlPathEscapesPercent(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	t.Setenv("TMPDIR", t.TempDir())
	e := NewRemoteExecutor("fe80::1%eth0", "u", WithPort(22))
	if !strings.Contains(e.controlPath, "%eth0") {
		t.Fatalf("controlPath %q: want the host's zone in the socket name", e.controlPath)
	}
	args := append([]string{"-G"}, e.baseArgs()...)
	out, err := exec.Command("ssh", append(args, "--", e.destination())...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "\ncontrolpath "+e.controlPath+"\n") {
		t.Fatalf("ssh -G = %s\nwant controlpath %s", out, e.controlPath)
	}
}
