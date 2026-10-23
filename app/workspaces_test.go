package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-215 end to end: POST /api/v1/workspaces' provisioner, through the
// real router and orchestrator, on a local target with a real tmux.
func TestProvisionWorkspace_EndToEnd(t *testing.T) {
	srv := fakeRouterServer(t)
	a, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()
	root := t.TempDir()
	target := &registry.Target{ID: uuid.NewString(), Name: "box", Kind: registry.TargetKindLocal, WorkspaceRoot: root}
	if err := a.store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}

	got, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: target.ID, Name: "api-made", Kind: "empty"})
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	if got.Existing || got.ConversationID == "" || got.Workspace.Status != registry.WorkspaceStatusProvisioning {
		t.Fatalf("result = %+v", got)
	}
	deadline := time.Now().Add(30 * time.Second)
	var ws *registry.Workspace
	for {
		if ws, err = a.store.GetWorkspace(ctx, got.Workspace.ID); err != nil {
			t.Fatal(err)
		}
		if ws.Status != registry.WorkspaceStatusProvisioning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still provisioning after 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if ws.Status != registry.WorkspaceStatusIdle || !strings.HasPrefix(ws.Path, resolvedRoot+string(filepath.Separator)) {
		t.Fatalf("provisioned = %s %q (%s), want idle under %s", ws.Status, ws.Path, ws.StatusReason, resolvedRoot)
	}
	if fi, err := os.Stat(ws.Path); err != nil || !fi.IsDir() {
		t.Errorf("the workspace directory: %v", err)
	}

	again, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: target.ID, Name: "api-made", Kind: "empty"})
	if err != nil || !again.Existing || again.Workspace.ID != ws.ID {
		t.Errorf("the same request again = %+v, %v", again, err)
	}
	if _, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: uuid.NewString(), Name: "x", Kind: "empty"}); !errors.Is(err, api.ErrNoSuchTarget) {
		t.Errorf("an unknown target: %v", err)
	}
	var specErr *api.WorkspaceSpecError
	if _, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: target.ID, Name: "Bad Name", Kind: "empty"}); !errors.As(err, &specErr) {
		t.Errorf("a bad name: %v", err)
	}
	var taken *api.WorkspaceNameTakenError
	if _, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: target.ID, Name: "api-made", Kind: "git_clone", GitRemote: "https://github.com/acme/x.git"}); !errors.As(err, &taken) {
		t.Errorf("the name from another source: %v", err)
	}
}

// LOOM-228 end to end: a git_worktree workspace from a repository under
// a local target's root, through the real router, orchestrator and tmux;
// then deleted with its worktree, the branch kept.
func TestWorktreeWorkspace_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv := fakeRouterServer(t)
	a, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()
	root := t.TempDir()
	base := filepath.Join(root, "server")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	git(base, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(base, "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(base, "add", "f")
	git(base, "commit", "-q", "-m", "f")
	target := &registry.Target{ID: uuid.NewString(), Name: "box", Kind: registry.TargetKindLocal, WorkspaceRoot: root}
	if err := a.store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}

	got, err := a.ProvisionWorkspace(ctx, api.WorkspaceToProvision{TargetID: target.ID, Name: "fix-1", Kind: "git_worktree",
		Worktree: api.WorktreeToAdd{Base: "server"}})
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var ws *registry.Workspace
	for {
		if ws, err = a.store.GetWorkspace(ctx, got.Workspace.ID); err != nil {
			t.Fatal(err)
		}
		if ws.Status != registry.WorkspaceStatusProvisioning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still provisioning after 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ws.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("provisioned = %s (%s)", ws.Status, ws.StatusReason)
	}
	if b := git(ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); b != "loomux/fix-1" {
		t.Errorf("branch = %q, want loomux/fix-1", b)
	}

	if _, err := a.DeleteWorkspaceAndWorktree(ctx, ws.ID); err != nil {
		t.Fatalf("DeleteWorkspaceAndWorktree: %v", err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if b := git(base, "branch", "--list", "loomux/fix-1"); b == "" {
		t.Error("the branch went with the worktree")
	}
}
