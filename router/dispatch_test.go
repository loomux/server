package router_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

func newTestStore(t *testing.T) registry.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath, sqlite.WithMasterKey([]byte("01234567890123456789012345678901"[:32])))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func createFixtureTarget(t *testing.T, store registry.Store) *registry.Target {
	t.Helper()
	target := &registry.Target{ID: uuid.NewString(), Name: "fixture-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}
	return target
}

func createFixtureWorkspace(t *testing.T, store registry.Store) *registry.Workspace {
	t.Helper()
	target := createFixtureTarget(t, store)
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "fixture-ws-" + t.Name(), TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}

// shortIdleAgentTypes is a registry with a short IdleTimeout for both a
// registered "claude-code" agent type and the "" key (shell-kind
// provisioning tasks always have AgentType ""), so fast unit tests don't
// wait out completion.Detector's 30s default.
func shortIdleAgentTypes(launchCommand string) router.AgentTypeRegistry {
	short := completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}
	return router.AgentTypeRegistry{
		"":            router.AgentType{AgentConfig: short},
		"claude-code": router.AgentType{AgentConfig: short, LaunchTemplate: launchCommand},
	}
}

// setup bundles the common test dependencies for router.Dispatch tests:
// a real (temp-file) store, a fake in-memory executor, a real
// completion.Detector (fast, via shortIdleAgentTypes), and a Router
// wired to all of it plus a StubRoutingModel the test controls.
func setup(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *routertest.StubRoutingModel) {
	t.Helper()
	store := newTestStore(t)
	exec, r, model := newRouter(t, store)
	return store, exec, r, model
}

// newRouter wires a Router around store exactly as setup does, passing
// opts through to router.New — for tests that need to wrap the store or
// supply an Option (e.g. WithLogger).
func newRouter(t *testing.T, store registry.Store, opts ...router.Option) (*fakeExecutor, *router.Router, *routertest.StubRoutingModel) {
	t.Helper()
	exec := newFakeExecutor()
	agentTypes := shortIdleAgentTypes("claude")
	markerDir := t.TempDir()
	// An unreachable fake target fails a wait in 100ms, not after the
	// production minute's grace (LOOM-135).
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir,
		completion.WithUnreachableGrace(100*time.Millisecond), completion.WithPollInterval(20*time.Millisecond))
	orch := orchestrator.New(store, exec.factory(), detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{}
	r := router.New(store, orch, exec.factory(), resolver, agentTypes, model, markerDir, opts...)
	return exec, r, model
}

func TestDispatch_AnswerDirectly(t *testing.T) {
	_, exec, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	reply, err := r.Dispatch(context.Background(), "conv-1", "what's up")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "the answer" {
		t.Fatalf("reply = %q, want %q", reply, "the answer")
	}
	if len(exec.sessions) != 0 {
		t.Fatalf("AnswerDirectly launched a session: %+v", exec.sessions)
	}
}

// TestDispatch_WorkspaceHint_ReachesRoutingModel proves Dispatch's
// variadic DispatchOption tail (LOOM-46) is threaded through to the
// routing model's Decide call unmodified — Router itself neither acts
// on nor validates the hint, that's the model's job.
func TestDispatch_WorkspaceHint_ReachesRoutingModel(t *testing.T) {
	_, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "what's up", router.WithWorkspaceHint("ws-hinted")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if model.LastDecideOptions.WorkspaceHint != "ws-hinted" {
		t.Fatalf("Decide's options.WorkspaceHint = %q, want %q", model.LastDecideOptions.WorkspaceHint, "ws-hinted")
	}
}

// TestDispatch_NoWorkspaceHint_OptionsAreZeroValue proves the ordinary,
// no-hint call path (every pre-LOOM-46 call site, including every other
// test in this file) still resolves to a zero-value DispatchOptions —
// the variadic tail being entirely optional wasn't just a compile-time
// nicety.
func TestDispatch_NoWorkspaceHint_OptionsAreZeroValue(t *testing.T) {
	_, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "what's up"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if o := model.LastDecideOptions; o.WorkspaceHint != "" || o.OpenTask != nil || len(o.History) != 0 {
		t.Fatalf("Decide's options = %+v, want zero value", model.LastDecideOptions)
	}
}

func TestDispatch_UseWorkspace(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	cred := &registry.Credential{ID: uuid.NewString(), Name: "MY_TOKEN", Value: "secret-value"}
	if err := store.CreateCredential(context.Background(), cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		if len(workspaces) != 1 || workspaces[0].ID != ws.ID {
			t.Fatalf("Decide received workspaces = %+v, want a snapshot of the fixture workspace", workspaces)
		}
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		if captured != exec.capture {
			t.Fatalf("Relay received %q, want the fake pane output %q", captured, exec.capture)
		}
		return router.RelayResult{Reply: "condensed reply", Done: true}, nil
	}

	reply, err := r.Dispatch(context.Background(), "conv-1", "do the thing")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "condensed reply" {
		t.Fatalf("reply = %q, want %q", reply, "condensed reply")
	}

	updatedWS, err := store.GetWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "condensed reply" {
		t.Fatalf("RollingSummary = %q, want %q", updatedWS.RollingSummary, "condensed reply")
	}

	// Exactly one session was created (the agent dispatch), kept after
	// the task completed for the grace period (LOOM-91).
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want exactly 1", exec.sessions)
	}
	var sess *fakeSession
	for _, s := range exec.sessions {
		sess = s
	}
	if !sess.alive {
		t.Fatalf("session torn down by Complete, want it kept for the grace period")
	}
	// The credential reaches the agent through an env file written over
	// RunOnce (stdin) and sourced by the session, never on its command
	// line (LOOM-113).
	if strings.Contains(sess.command, "secret-value") || !strings.Contains(sess.command, ".loomux/env/") {
		t.Fatalf("session command = %q, want it to source the env file and not carry the value", sess.command)
	}
	var wrote bool
	for _, c := range exec.runOnceCommands {
		wrote = wrote || (strings.Contains(c, ".loomux/env/") && strings.Contains(c, "MY_TOKEN='secret-value'"))
	}
	if !wrote {
		t.Fatalf("RunOnce scripts = %q, want the env file written with MY_TOKEN", exec.runOnceCommands)
	}
	if !strings.Contains(sess.command, "claude") {
		t.Fatalf("session command = %q, want it to contain the resolved launch template", sess.command)
	}
}

func TestDispatch_ProvisionWorkspace(t *testing.T) {
	store, exec, r, model := setup(t)
	target := createFixtureTarget(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action: router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{
				Name:     "new-ws",
				TargetID: target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://example.invalid/repo.git",
			},
			AgentType: "claude-code",
		}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "provisioned and dispatched", Done: true}, nil
	}

	reply, err := r.Dispatch(context.Background(), "conv-1", "clone https://example.invalid/repo.git into a new workspace")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "provisioned and dispatched" {
		t.Fatalf("reply = %q, want %q", reply, "provisioned and dispatched")
	}

	list, err := store.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 1 || list[0].Name != "new-ws" {
		t.Fatalf("ListWorkspaces = %+v, want a single workspace named new-ws", list)
	}
	if !list[0].IsDynamic {
		t.Fatalf("provisioned workspace IsDynamic = false, want true")
	}
	if list[0].Status != registry.WorkspaceStatusIdle {
		t.Fatalf("provisioned workspace Status = %q, want %q (Complete reverts it after the agent turn)", list[0].Status, registry.WorkspaceStatusIdle)
	}

	// Two sessions: the shell-kind provisioning task, then the
	// agent-kind dispatch task. The provisioning pane is torn down when
	// its command finishes; the agent's is kept for the grace period
	// (LOOM-91).
	if len(exec.sessions) != 2 {
		t.Fatalf("sessions = %+v, want exactly 2 (provisioning + agent dispatch)", exec.sessions)
	}
	alive := 0
	for _, s := range exec.sessions {
		if s.alive {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("%d sessions alive after Dispatch, want 1 (the completed agent's, in its grace period)", alive)
	}
}

func TestDispatch_UnknownAgentType(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "no-such-agent-type"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err == nil {
		t.Fatalf("Dispatch with an unknown agent type: got nil error")
	}
	if len(exec.sessions) != 0 {
		t.Fatalf("a session was launched despite an unknown agent type: %+v", exec.sessions)
	}
}

func TestDispatch_UnknownWorkspace(t *testing.T) {
	_, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: "does-not-exist", AgentType: "claude-code"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err == nil {
		t.Fatalf("Dispatch against an unknown workspace: got nil error")
	}
}

func TestDispatch_ProvisioningFailure_LeavesWorkspaceRowInPlace(t *testing.T) {
	store, exec, r, model := setup(t)
	target := createFixtureTarget(t, store)
	exec.unreachable = true // the provisioning task's NewSession will fail

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action: router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{
				Name:     "doomed-ws",
				TargetID: target.ID, Kind: router.ProvisionEmpty,
			},
			AgentType: "claude-code",
		}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "start a new workspace"); err == nil {
		t.Fatalf("Dispatch with a failing provisioning task: got nil error")
	}

	list, err := store.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 1 || list[0].Name != "doomed-ws" {
		t.Fatalf("ListWorkspaces = %+v, want the workspace row left in place after a provisioning failure", list)
	}
	// LOOM-71: kept for inspection, but marked failed — not idle, active or
	// provisioning.
	if list[0].Status != registry.WorkspaceStatusFailed {
		t.Fatalf("workspace status = %q, want %q", list[0].Status, registry.WorkspaceStatusFailed)
	}
}

// LOOM-113 review: RunOnce's errors quote the script they ran, and the
// env-file script holds the secrets. A failed write must not carry them
// into the dispatch error, the logs or a task's failure reason.
func TestDispatch_EnvFileWriteFailureLeaksNoSecret(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	if err := store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "MY_TOKEN", Value: "sk-PROBE-secret-value-123",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	exec.runOnce = func(command string) (string, error) {
		if strings.Contains(command, ".loomux/env/") && strings.Contains(command, "cat >") {
			return "mkdir: cannot create directory: Not a directory\n", fmt.Errorf("targets: run once: %s: exit status 1", command)
		}
		return "", nil
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "do the thing")
	if err == nil {
		t.Fatal("Dispatch succeeded with the env file unwritten")
	}
	if strings.Contains(err.Error(), "sk-PROBE") {
		t.Errorf("dispatch error carries the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "Not a directory") {
		t.Errorf("dispatch error = %v, want the target's own reason", err)
	}
	tasks, _ := store.ListTasks(context.Background())
	for _, task := range tasks {
		if strings.Contains(task.FailureReason, "sk-PROBE") {
			t.Errorf("task failure reason carries the secret: %q", task.FailureReason)
		}
	}
}

// LOOM-85 review: an unreachable error from the env-file write keeps its
// class but not its Detail, which is the remote shell's stderr when the
// script itself exited 255 and so could echo the secrets.
func TestDispatch_EnvFileWriteUnreachableLeaksNoSecret(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	if err := store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "MY_TOKEN", Value: "sk-PROBE-secret-value-123",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	exec.runOnce = func(command string) (string, error) {
		if strings.Contains(command, ".loomux/env/") && strings.Contains(command, "cat >") {
			return "", &targets.UnreachableError{Host: "jet01", Failure: targets.SSHOther, Detail: "fish: unknown command: MY_TOKEN='sk-PROBE-secret-value-123'"}
		}
		return "", nil
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "do the thing")
	if err == nil {
		t.Fatal("Dispatch succeeded with the env file unwritten")
	}
	if strings.Contains(err.Error(), "sk-PROBE") {
		t.Errorf("dispatch error carries the secret: %v", err)
	}
	if u, ok := targets.AsUnreachable(err); !ok || u.Failure != targets.SSHOther {
		t.Errorf("dispatch error = %v, want the unreachable class kept", err)
	}
	tasks, _ := store.ListTasks(context.Background())
	for _, task := range tasks {
		if strings.Contains(task.FailureReason, "sk-PROBE") {
			t.Errorf("task failure reason carries the secret: %q", task.FailureReason)
		}
	}
}
