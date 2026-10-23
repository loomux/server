package orchestrator_test

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/orchestrator/detectortest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// keyedSetup is setup over a store with a vault key, so credentials can
// be made.
func keyedSetup(t *testing.T) (registry.Store, *registry.Workspace, *fakeExecutor, *orchestrator.Orchestrator) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"), sqlite.WithMasterKey([]byte("01234567890123456789012345678901")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ws := createFixtureWorkspace(t, store)
	fx := newFakeExecutor()
	return store, ws, fx, orchestrator.New(store, fx.factory(), detectortest.NewManualDetector())
}

// git runs git in dir with no user or system config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// worktreeOf makes a repository with one commit under a temp root and a
// worktree of it on branch loomux/<name>, returning both directories.
func worktreeOf(t *testing.T, name string) (base, wt string) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	base, wt = filepath.Join(root, "server"), filepath.Join(root, name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, base, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(base, "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, base, "add", "f")
	git(t, base, "commit", "-q", "-m", "f")
	git(t, base, "worktree", "add", "-q", "-b", "loomux/"+name, wt)
	return base, wt
}

// atPath points ws at dir.
func atPath(t *testing.T, store registry.Store, ws *registry.Workspace, dir string) {
	t.Helper()
	ws.Path = dir
	if err := store.UpdateWorkspace(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
}

func gone(t *testing.T, store registry.Store, id string) bool {
	t.Helper()
	_, err := store.GetWorkspace(context.Background(), id)
	return errors.Is(err, registry.ErrNotFound)
}

// LOOM-228: deleting a git_worktree workspace with its worktree removes
// the worktree from the target (the repository forgets it, the branch
// stays) and then the rows.
func TestDeleteWorkspaceAndWorktree_Removes(t *testing.T) {
	store, ws, fx, _, o := setup(t)
	fx.runOnceShell = true
	base, wt := worktreeOf(t, "fix-1")
	atPath(t, store, ws, wt)
	if _, err := o.DeleteWorkspaceAndWorktree(context.Background(), ws.ID); err != nil {
		t.Fatalf("DeleteWorkspaceAndWorktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree directory is still there: %v", err)
	}
	if list := git(t, base, "worktree", "list"); strings.Contains(list, "fix-1") {
		t.Errorf("the repository still lists the worktree: %s", list)
	}
	if b := git(t, base, "branch", "--list", "loomux/fix-1"); b == "" {
		t.Error("the worktree's branch was deleted; it should stay")
	}
	if !gone(t, store, ws.ID) {
		t.Error("the workspace row is still there")
	}
}

// A plain delete leaves the worktree on the target, as it leaves any
// workspace's files.
func TestDeleteWorkspace_LeavesTheWorktree(t *testing.T) {
	store, ws, fx, _, o := setup(t)
	fx.runOnceShell = true
	_, wt := worktreeOf(t, "fix-2")
	atPath(t, store, ws, wt)
	if _, err := o.DeleteWorkspace(context.Background(), ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("a plain delete touched the worktree: %v", err)
	}
}

// What git or the check refuses is a conflict in their words, and then
// nothing is deleted: a worktree with work not committed, a repository's
// own checkout, a directory that isn't git, a credential still scoped to
// the workspace (checked before git touches anything).
func TestDeleteWorkspaceAndWorktree_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, store registry.Store, ws *registry.Workspace) (dir string)
		want  string
	}{
		{"untracked files", func(t *testing.T, store registry.Store, ws *registry.Workspace) string {
			_, wt := worktreeOf(t, "dirty")
			if err := os.WriteFile(filepath.Join(wt, "notes"), []byte("wip"), 0o644); err != nil {
				t.Fatal(err)
			}
			return wt
		}, "untracked"},
		{"the repository's own checkout", func(t *testing.T, store registry.Store, ws *registry.Workspace) string {
			base, _ := worktreeOf(t, "other")
			return base
		}, "not a worktree"},
		{"not git at all", func(t *testing.T, store registry.Store, ws *registry.Workspace) string {
			dir := t.TempDir()
			t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
			return dir
		}, "isn't a git checkout"},
		{"a credential scoped to it", func(t *testing.T, store registry.Store, ws *registry.Workspace) string {
			_, wt := worktreeOf(t, "scoped")
			if err := store.CreateCredential(context.Background(), &registry.Credential{ID: "c1", Name: "TOKEN", Value: "v", WorkspaceID: ws.ID}); err != nil {
				t.Fatal(err)
			}
			return wt
		}, "credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ws, fx, o := keyedSetup(t)
			fx.runOnceShell = true
			dir := tc.setup(t, store, ws)
			atPath(t, store, ws, dir)
			_, err := o.DeleteWorkspaceAndWorktree(context.Background(), ws.ID)
			var conflict *registry.ConflictError
			if !errors.As(err, &conflict) || !strings.Contains(conflict.Reason, tc.want) {
				t.Fatalf("err = %v, want a conflict saying %q", err, tc.want)
			}
			if gone(t, store, ws.ID) {
				t.Error("the workspace was deleted though its worktree wasn't removed")
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("the directory went: %v", err)
			}
		})
	}
}

// A worktree directory already gone counts as removed; a target that
// doesn't answer is an error, not a conflict, and deletes nothing.
func TestDeleteWorkspaceAndWorktree_GoneOrUnreachable(t *testing.T) {
	store, ws, fx, _, o := setup(t)
	fx.runOnceShell = true
	atPath(t, store, ws, filepath.Join(t.TempDir(), "never-there"))
	if _, err := o.DeleteWorkspaceAndWorktree(context.Background(), ws.ID); err != nil || !gone(t, store, ws.ID) {
		t.Errorf("a worktree already gone: err %v, row gone %v", err, gone(t, store, ws.ID))
	}

	store, ws, fx, _, o = setup(t)
	_, wt := worktreeOf(t, "far")
	atPath(t, store, ws, wt)
	fx.unreachable = true
	_, err := o.DeleteWorkspaceAndWorktree(context.Background(), ws.ID)
	var conflict *registry.ConflictError
	if err == nil || errors.As(err, &conflict) || gone(t, store, ws.ID) {
		t.Errorf("unreachable: err %v, row gone %v; want an error and the row kept", err, gone(t, store, ws.ID))
	}
}
