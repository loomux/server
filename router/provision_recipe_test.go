package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

func TestProvisionSpecValidate(t *testing.T) {
	ok := []ProvisionSpec{
		{Name: "hostname-uptime", TargetID: "t", Kind: ProvisionEmpty},
		// Underscores, as in names given before this rule (LOOM-130).
		{Name: "jet01_connectivity_test", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "api", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "https://github.com/loomux/server.git"},
		{Name: "api2", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "git@github.com:loomux/server.git"},
		{Name: "api3", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "ssh://git@example.com:22/loomux/server.git"},
		{Name: "existing", TargetID: "t", Kind: ProvisionExistingDir},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want ok", s, err)
		}
	}
	bad := []ProvisionSpec{
		{Name: "", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "../escape", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "~", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "/etc", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "a/b", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "Hostname_Uptime", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "-rf", TargetID: "t", Kind: ProvisionEmpty},
		{Name: "_hidden", TargetID: "t", Kind: ProvisionEmpty},
		{Name: strings.Repeat("a", 64), TargetID: "t", Kind: ProvisionEmpty},
		{Name: "x", TargetID: "t", Kind: "run_anything"},
		{Name: "x", TargetID: "t", Kind: ""},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "ext::sh -c touch% /tmp/pwn"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "file:///etc"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "--upload-pack=touch /tmp/pwn"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "https://example.com/a b"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "https://example.com/$(id)"},
		{Name: "x", TargetID: "t", Kind: ProvisionEmpty, GitRemote: "https://github.com/x/y"},
		// Option-like user or host parts (re-review): ssh would read them
		// as options.
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "-u@h:p"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "--upload-pack@h:x"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "h@-oProxyCommand:x"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "ssh://-oProxyCommand/x"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "ssh://git@-oProxyCommand/x"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "https://-x.example.com/repo"},
		{Name: "x", TargetID: "t", Kind: ProvisionGitClone, GitRemote: "git@github.com:-oProxyCommand=x"},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want rejected", s)
		}
	}
}

// runRecipe runs the provisioning recipe through a real sh, as a target
// would, and returns its combined output and exit code.
func runRecipe(t *testing.T, target *registry.Target, spec ProvisionSpec, home string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", provisioningRecipe(target, spec))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("run recipe: %v", err)
	}
	return string(out), 0
}

// The recipe Loomux builds itself (LOOM-90) confines a workspace to the
// target's root — following symlinks — and reports the directory it
// resolved, which is what the workspace's path becomes.
func TestProvisioningRecipe_RealShell(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "root")
	configured := &registry.Target{Name: "t", WorkspaceRoot: root}
	defaulted := &registry.Target{Name: "t"}
	realRoot := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("empty under configured root", func(t *testing.T) {
		out, code := runRecipe(t, configured, ProvisionSpec{Name: "ws1", Kind: ProvisionEmpty}, home)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		want := filepath.Join(realRoot(root), "ws1")
		if got := parseProvisionedPath(out); got != want {
			t.Errorf("resolved path = %q, want %q (output %q)", got, want, out)
		}
		if st, err := os.Stat(want); err != nil || !st.IsDir() {
			t.Errorf("workspace dir not created: %v", err)
		}
	})
	t.Run("empty under default root", func(t *testing.T) {
		out, code := runRecipe(t, defaulted, ProvisionSpec{Name: "ws2", Kind: ProvisionEmpty}, home)
		want := filepath.Join(realRoot(home), "loomux-workspaces", "ws2")
		if code != 0 || parseProvisionedPath(out) != want {
			t.Errorf("exit %d, output %q; want %q", code, out, want)
		}
	})
	t.Run("existing_dir that doesn't exist", func(t *testing.T) {
		out, code := runRecipe(t, configured, ProvisionSpec{Name: "nope", Kind: ProvisionExistingDir}, home)
		if code == 0 || parseProvisionedPath(out) != "" {
			t.Errorf("exit %d, output %q; want a failure", code, out)
		}
	})
	t.Run("symlink escaping the root", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sneaky")); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []ProvisionKind{ProvisionEmpty, ProvisionExistingDir} {
			out, code := runRecipe(t, configured, ProvisionSpec{Name: "sneaky", Kind: kind}, home)
			if code == 0 || parseProvisionedPath(out) != "" || !strings.Contains(out, "outside the workspace root") {
				t.Errorf("%s: exit %d, output %q; want refused as outside the root", kind, code, out)
			}
		}
	})
	t.Run("git_clone onto an existing directory", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "taken"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, code := runRecipe(t, configured, ProvisionSpec{Name: "taken", Kind: ProvisionGitClone,
			GitRemote: "https://example.invalid/x.git"}, home)
		if code == 0 || !strings.Contains(out, "already exists") {
			t.Errorf("exit %d, output %q; want refused", code, out)
		}
	})
}

// A root containing shell metacharacters is data, not code.
func TestProvisioningRecipe_QuotesRoot(t *testing.T) {
	base := t.TempDir()
	marker := filepath.Join(base, "injected")
	root := filepath.Join(base, `r'$(touch `+marker+`)'`)
	out, code := runRecipe(t, &registry.Target{Name: "t", WorkspaceRoot: root}, ProvisionSpec{Name: "w", Kind: ProvisionEmpty}, t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the root's $(...) ran")
	}
}
