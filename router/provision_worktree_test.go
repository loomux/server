package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

// gitIn runs git in dir, failing the test on an error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// baseRepo makes a repository named name under root with two commits
// on main and a branch "release" at the first.
func baseRepo(t *testing.T, root, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	for _, f := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", f)
		gitIn(t, dir, "commit", "-q", "-m", f)
		if f == "one" {
			gitIn(t, dir, "branch", "release")
		}
	}
	return dir
}

// LOOM-228: a git_worktree workspace is `git worktree add -b` from a
// repository already under the root: its own directory and branch,
// confined like any workspace, and every refusal git or the recipe makes
// is the provisioning's failure, in its output.
func TestProvisioningRecipe_GitWorktree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	target := &registry.Target{Name: "t", WorkspaceRoot: root}
	base := baseRepo(t, root, "server")
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	// The recipe's git runs with the test's environment: no user config.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	worktree := func(name string, w WorktreeSpec) ProvisionSpec {
		return ProvisionSpec{Name: name, Kind: ProvisionGitWorktree, Worktree: w}
	}

	t.Run("a new branch from HEAD, named for the workspace", func(t *testing.T) {
		out, code := runRecipe(t, target, worktree("fix-1", WorktreeSpec{Base: "server"}), t.TempDir())
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		want := filepath.Join(realRoot, "fix-1")
		if got := parseProvisionedPath(out, testNonce); got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if b := gitIn(t, want, "rev-parse", "--abbrev-ref", "HEAD"); b != "loomux/fix-1" {
			t.Errorf("branch = %q, want loomux/fix-1", b)
		}
		if head, main := gitIn(t, want, "rev-parse", "HEAD"), gitIn(t, base, "rev-parse", "main"); head != main {
			t.Errorf("the worktree starts at %s, want main's %s", head, main)
		}
		if list := gitIn(t, base, "worktree", "list"); !strings.Contains(list, want) {
			t.Errorf("the base doesn't list the worktree: %s", list)
		}
	})
	t.Run("a named branch from a ref", func(t *testing.T) {
		out, code := runRecipe(t, target, worktree("fix-2", WorktreeSpec{Base: "server", Ref: "release", Branch: "orski/loom-228-x"}), t.TempDir())
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		dir := filepath.Join(realRoot, "fix-2")
		if b := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "orski/loom-228-x" {
			t.Errorf("branch = %q", b)
		}
		if head, rel := gitIn(t, dir, "rev-parse", "HEAD"), gitIn(t, base, "rev-parse", "release"); head != rel {
			t.Errorf("the worktree starts at %s, want release's %s", head, rel)
		}
	})
	for _, tc := range []struct {
		name string
		spec ProvisionSpec
		want string // in the output
	}{
		{"the branch exists already", worktree("fix-3", WorktreeSpec{Base: "server", Branch: "release"}), "already exists"},
		{"the ref doesn't exist", worktree("fix-4", WorktreeSpec{Base: "server", Ref: "nope"}), "nope"},
		{"no such base", worktree("fix-5", WorktreeSpec{Base: "absent"}), "no repository directory"},
		{"the directory is taken", worktree("server", WorktreeSpec{Base: "fix-1"}), "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runRecipe(t, target, tc.spec, t.TempDir())
			if code == 0 || parseProvisionedPath(out, testNonce) != "" || !strings.Contains(out, tc.want) {
				t.Errorf("exit %d, output %q; want a failure saying %q", code, out, tc.want)
			}
		})
	}
	t.Run("a base that isn't a repository", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_CEILING_DIRECTORIES", realRoot) // the temp dir may sit in a repository
		out, code := runRecipe(t, target, worktree("fix-6", WorktreeSpec{Base: "plain"}), t.TempDir())
		if code == 0 || !strings.Contains(out, "isn't a git repository") {
			t.Errorf("exit %d, output %q; want refused", code, out)
		}
	})
	t.Run("a base symlinked out of the root", func(t *testing.T) {
		outside := baseRepo(t, t.TempDir(), "elsewhere")
		if err := os.Symlink(outside, filepath.Join(root, "sneaky")); err != nil {
			t.Fatal(err)
		}
		out, code := runRecipe(t, target, worktree("fix-7", WorktreeSpec{Base: "sneaky"}), t.TempDir())
		if code == 0 || !strings.Contains(out, "outside the workspace root") {
			t.Errorf("exit %d, output %q; want refused", code, out)
		}
		if list := gitIn(t, outside, "worktree", "list"); strings.Contains(list, "fix-7") {
			t.Errorf("a worktree was added to the repository outside the root: %s", list)
		}
	})
}

func TestProvisionSpecValidate_GitWorktree(t *testing.T) {
	ok := []ProvisionSpec{
		{Name: "fix-1", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server"}},
		{Name: "fix-2", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "origin/main", Branch: "orski/loom-228-worktree"}},
		{Name: "fix-3", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "v0.3.89"}},
		{Name: "fix-4", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "9fc681a"}},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want ok", s, err)
		}
	}
	bad := []ProvisionSpec{
		{Name: "x", Kind: ProvisionGitWorktree},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "../etc"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "a/b"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "x"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server"}, GitRemote: "https://github.com/x/y"},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "--upload-pack=x"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "a..b"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Ref: "$(id)"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Branch: "-b"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Branch: "a b"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Branch: "x.lock"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Branch: "a/.hidden"}},
		{Name: "x", Kind: ProvisionGitWorktree, Worktree: WorktreeSpec{Base: "server", Branch: "trailing/"}},
		{Name: "x", Kind: ProvisionEmpty, Worktree: WorktreeSpec{Base: "server"}},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want rejected", s)
		}
	}
}
