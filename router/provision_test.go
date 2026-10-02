package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// LOOM-90: provisioning runs only the recipe Loomux builds from validated
// fields — nothing the routing model wrote — and the agent then starts in
// the directory that recipe resolved inside the workspace root.
func TestProvision_RunsOnlyTheGoBuiltRecipe(t *testing.T) {
	store, exec, r, model := setup(t)
	target := createFixtureTarget(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "claude-code", NewWorkspace: router.ProvisionSpec{
			Name: "server", TargetID: target.ID, Kind: router.ProvisionGitClone,
			GitRemote: "https://github.com/loomux/server.git",
		}}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "ok", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "clone loomux/server and look around"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var recipe string
	var agentDir string
	exec.mu.Lock()
	for _, s := range exec.sessions {
		if isProvisioning(s.command) {
			recipe = s.command
		} else {
			agentDir = s.dir
		}
	}
	exec.mu.Unlock()
	if !strings.Contains(recipe, "git clone -- 'https://github.com/loomux/server.git'") {
		t.Errorf("provisioning recipe doesn't clone the validated remote:\n%s", recipe)
	}
	if agentDir != "/fake-root/server" {
		t.Errorf("agent started in %q, want the directory provisioning resolved", agentDir)
	}
	list, _ := store.ListWorkspaces(context.Background())
	if len(list) != 1 || list[0].Path != "/fake-root/server" || list[0].Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspaces = %+v, want one at the resolved path", list)
	}
	tasks, _ := store.ListTasksByWorkspace(context.Background(), list[0].ID)
	for _, task := range tasks {
		if task.Kind == registry.TaskKindCommand && (task.Command != recipe || task.ExitCode == nil || *task.ExitCode != 0) {
			t.Errorf("provisioning task = %+v, want the recipe recorded with exit 0", task)
		}
	}
}

// An invalid spec — a path-like or non-slug name, an unknown kind, a
// clone of a dangerous remote — is refused before anything is written or
// run.
func TestProvision_InvalidSpecWritesNothing(t *testing.T) {
	for _, spec := range []router.ProvisionSpec{
		{Name: "~", Kind: router.ProvisionEmpty},
		{Name: "../../.ssh", Kind: router.ProvisionEmpty},
		{Name: "x", Kind: router.ProvisionGitClone, GitRemote: "ext::sh -c touch% /tmp/pwn"},
		{Name: "x", Kind: "shell"},
	} {
		store, exec, r, model := setup(t)
		target := createFixtureTarget(t, store)
		spec.TargetID = target.ID
		model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "claude-code", NewWorkspace: spec}, nil
		}
		if _, err := r.Dispatch(context.Background(), "conv-1", "go"); err == nil {
			t.Errorf("spec %+v: Dispatch succeeded, want refused", spec)
		}
		if list, _ := store.ListWorkspaces(context.Background()); len(list) != 0 {
			t.Errorf("spec %+v: workspace rows written: %+v", spec, list)
		}
		if cmds := exec.launchedCommands(); len(cmds) != 0 {
			t.Errorf("spec %+v: ran %v", spec, cmds)
		}
	}
}

// A confirmed install offer that will also provision a workspace says so
// — the offer shows everything that will run, provisioning included.
func TestInstallOffer_DisclosesProvisioning(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.decide(router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "codex", NewWorkspace: router.ProvisionSpec{
		Name: "server", TargetID: h.target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://github.com/loomux/server.git",
	}})
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "clone loomux/server with codex")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, want := range []string{router.ProvisioningMarker, "git clone -- 'https://github.com/loomux/server.git'", codexInstall} {
		if !strings.Contains(reply, want) {
			t.Errorf("offer doesn't disclose %q:\n%s", want, reply)
		}
	}
}

// Redaction happens before output is cut to size, so a secret straddling
// the cut can't leak its tail; and it covers provisioning and install
// output too, not only an agent's.
func TestOutputRedactedBeforeBounding(t *testing.T) {
	const secret = "sk-live-0123456789-abcdefghij"
	// The secret sits at the very start of the last 4000 bytes' window
	// minus a few: bounding first would keep only its tail.
	straddling := secret + strings.Repeat("x", 3990)

	t.Run("agent exit", func(t *testing.T) {
		h := newRouterHarness(t)
		addSecret(t, h.store, "claude-code", secret)
		h.exec.paneExit = func(command string) *targets.PaneExit {
			if strings.HasSuffix(command, "claude") {
				return &targets.PaneExit{Status: 1, Output: straddling}
			}
			return nil
		}
		_, err := h.r.Dispatch(context.Background(), "conv-1", "go")
		if err == nil {
			t.Fatal("Dispatch: nil error")
		}
		assertNoSecretFragment(t, err.Error(), secret)
		assertNoSecretFragment(t, onlyTask(t, h.store, h.ws.ID).OutputTail, secret)
	})

	t.Run("provisioning failure", func(t *testing.T) {
		store, exec, r, model := setup(t)
		target := createFixtureTarget(t, store)
		addSecret(t, store, "", secret)
		exec.paneExit = func(command string) *targets.PaneExit {
			if isProvisioning(command) {
				return &targets.PaneExit{Status: 128, Output: "fatal: auth failed for " + straddling}
			}
			return nil
		}
		model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "claude-code", NewWorkspace: router.ProvisionSpec{
				Name: "ws", TargetID: target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://example.com/x.git",
			}}, nil
		}
		_, err := r.Dispatch(context.Background(), "conv-1", "go")
		if err == nil {
			t.Fatal("Dispatch: nil error")
		}
		assertNoSecretFragment(t, err.Error(), secret)
		list, _ := store.ListWorkspaces(context.Background())
		assertNoSecretFragment(t, list[0].StatusReason, secret)
		for _, task := range mustTasks(t, store, list[0].ID) {
			assertNoSecretFragment(t, task.OutputTail+task.FailureReason, secret)
		}
	})

	t.Run("install failure", func(t *testing.T) {
		h := newAvailabilityHarness(t)
		addSecret(t, h.store, "", secret)
		ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
		if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
		h.exec.paneExit = func(command string) *targets.PaneExit {
			if command == codexInstall {
				return &targets.PaneExit{Status: 1, Output: straddling}
			}
			return nil
		}
		if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
			t.Fatalf("Dispatch (offer): %v", err)
		}
		reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes")
		if err != nil {
			t.Fatalf("Dispatch (confirm): %v", err)
		}
		assertNoSecretFragment(t, reply, secret)
	})
}

func addSecret(t *testing.T, store registry.Store, agentType, value string) {
	t.Helper()
	if err := store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "SECRET_" + strings.ToUpper(strings.ReplaceAll(agentType, "-", "_")), AgentType: agentType, Value: value,
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
}

// assertNoSecretFragment fails if text carries the secret or any
// 8-character tail of it.
func assertNoSecretFragment(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret[len(secret)-8:]) {
		t.Errorf("text leaks (part of) a secret: …%s", text[max(0, strings.Index(text, secret[len(secret)-8:])-20):][:40])
	}
}

func mustTasks(t *testing.T, store registry.Store, workspaceID string) []*registry.Task {
	t.Helper()
	tasks, err := store.ListTasksByWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	return tasks
}
