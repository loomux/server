package router_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// profileRouter wires a Router whose "claude-code" entry is at, plus a
// relay that never finishes the task (so a second Dispatch continues
// the same one).
func profileRouter(t *testing.T, at router.AgentType) (registry.Store, *registry.Workspace, *fakeExecutor, *router.Router) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	at.AgentConfig = completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}
	agentTypes := router.AgentTypeRegistry{
		"":            router.AgentType{AgentConfig: at.AgentConfig},
		"claude-code": at,
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "working", Done: false}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	return store, ws, exec, r
}

func onlySession(t *testing.T, exec *fakeExecutor) *fakeSession {
	t.Helper()
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(exec.sessions))
	}
	for _, s := range exec.sessions {
		return s
	}
	return nil
}

// TestDispatch_PromptAsArg_FirstTurnIsAnArgumentNotKeys: the first
// turn's message goes on the launch command line (LOOM-78), so it can't
// be lost to a TUI that isn't ready or eaten by a trust dialog. Later
// turns still go through send-keys — by then the previous turn's
// completion signal has shown the agent is ready.
func TestDispatch_PromptAsArg_FirstTurnIsAnArgumentNotKeys(t *testing.T) {
	_, _, exec, r := profileRouter(t, router.AgentType{
		LaunchTemplate: "claude",
		Profile:        router.LaunchProfile{PromptAsArg: true},
	})

	if _, err := r.Dispatch(context.Background(), "conv-1", "write hello.txt"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	sess := onlySession(t, exec)
	if !strings.HasSuffix(sess.command, `claude '--' 'write hello.txt'`) {
		t.Fatalf("command = %q, want the message as a positional argument after --", sess.command)
	}
	if len(sess.keys) != 0 {
		t.Fatalf("keys = %q, want none on turn 1 (the prompt was an argument)", sess.keys)
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "now append world"); err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	if len(sess.keys) != 1 || sess.keys[0] != "now append world" {
		t.Fatalf("keys = %q, want the follow-up turn sent as keys", sess.keys)
	}
}

func TestDispatch_NoPromptAsArg_FirstTurnIsKeys(t *testing.T) {
	_, _, exec, r := profileRouter(t, router.AgentType{LaunchTemplate: "claude"})
	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	sess := onlySession(t, exec)
	if !strings.HasSuffix(sess.command, "claude") {
		t.Fatalf("command = %q, want the bare template", sess.command)
	}
	if len(sess.keys) != 1 || sess.keys[0] != "hi" {
		t.Fatalf("keys = %q, want [hi]", sess.keys)
	}
}

// TestDispatch_ProfileArgOrder pins the command shape: template,
// permission args, workspace trust args, completion hook args, then
// "--" and the prompt.
func TestDispatch_ProfileArgOrder(t *testing.T) {
	_, ws, exec, r := profileRouter(t, router.AgentType{
		LaunchTemplate:     "agent",
		CompletionHookArgs: []string{"--hook"},
		Profile: router.LaunchProfile{
			PermissionArgs: []string{"--perm", "x"},
			TrustArgs:      func(dir string) []string { return []string{"--trust", dir} },
			PromptAsArg:    true,
		},
	})
	if _, err := r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := `agent '--perm' 'x' '--trust' '` + ws.Path + `' '--hook' '--' 'go'`
	if sess := onlySession(t, exec); !strings.HasSuffix(sess.command, want) {
		t.Fatalf("command = %q, want suffix %q", sess.command, want)
	}
}

// TestDispatch_PromptAsArg_RoundTripsThroughShell runs the real launch
// command under sh with a template that prints its arguments: a
// message full of shell metacharacters must arrive as exactly one,
// unmodified argument.
func TestDispatch_PromptAsArg_RoundTripsThroughShell(t *testing.T) {
	msg := "it's $(touch /tmp/pwned) `x` \"q\" -- --flag\nline two; rm -rf ~"
	_, _, fe, r := profileRouter(t, router.AgentType{
		LaunchTemplate: `printf '<%s>'`,
		Profile:        router.LaunchProfile{PromptAsArg: true},
	})
	if _, err := r.Dispatch(context.Background(), "conv-1", msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	out, err := exec.Command("sh", "-c", onlySession(t, fe).command).Output()
	if err != nil {
		t.Fatalf("sh -c: %v", err)
	}
	if want := "<-->" + "<" + msg + ">"; string(out) != want {
		t.Fatalf("args = %q, want %q", out, want)
	}
}

func TestApplyProfileOverrides(t *testing.T) {
	reg := router.AgentTypeRegistry{
		"claude-code": router.AgentType{
			LaunchTemplate: "claude",
			Profile: router.LaunchProfile{
				PermissionArgs: []string{"--permission-mode", "acceptEdits"},
				TrustArgs:      func(dir string) []string { return []string{"--trust", dir} },
				PromptAsArg:    true,
			},
		},
		"codex": router.AgentType{LaunchTemplate: "codex", Profile: router.LaunchProfile{PermissionArgs: []string{"-a", "on-request"}}},
	}
	none := []string{}
	off := false
	err := reg.ApplyProfileOverrides(map[string]router.ProfileOverride{
		"claude-code": {PermissionArgs: &[]string{"--permission-mode", "plan"}, PreTrust: &off, PromptAsArg: &off},
		"codex":       {PermissionArgs: &none},
	})
	if err != nil {
		t.Fatalf("ApplyProfileOverrides: %v", err)
	}
	cc := reg["claude-code"].Profile
	if strings.Join(cc.PermissionArgs, " ") != "--permission-mode plan" || cc.TrustArgs != nil || cc.PromptAsArg {
		t.Fatalf("claude-code profile = %+v, want overrides applied", cc)
	}
	if got := reg["codex"].Profile.PermissionArgs; len(got) != 0 {
		t.Fatalf("codex PermissionArgs = %q, want explicitly emptied", got)
	}

	if err := reg.ApplyProfileOverrides(map[string]router.ProfileOverride{"gemini": {}}); err == nil {
		t.Fatal("override for an unregistered agent type: want error, got nil")
	}
}
