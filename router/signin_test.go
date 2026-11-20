package router_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

const (
	claudeSignedOut = `{"loggedIn": false, "authMethod": "none"}`
	claudeSignedIn  = `{"loggedIn": true, "authMethod": "claude.ai", "subscriptionType": "max"}`
	signInSession   = "loomux-signin-claude-code"
)

func (p *probeScript) setAuth(bin, out string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.auth == nil {
		p.auth = map[string]string{}
	}
	p.auth[bin] = out
}

// fakeAuthDetails stands in for an adapter's Details: the method and
// plan as the test's auth output names them.
func fakeAuthDetails(out string) router.AuthDetails {
	var d router.AuthDetails
	if m := regexp.MustCompile(`"authMethod":\s*"([^"]*)"`).FindStringSubmatch(out); m != nil {
		d.Method = m[1]
	}
	if m := regexp.MustCompile(`"subscriptionType":\s*"([^"]*)"`).FindStringSubmatch(out); m != nil {
		d.Plan = m[1]
	}
	return d
}

// withSignIn gives claude-code a sign-in command, credential variables
// and auth details (LOOM-316).
func withSignIn(types router.AgentTypeRegistry) {
	at := types["claude-code"]
	check := *at.AuthCheck
	check.Details = fakeAuthDetails
	at.AuthCheck = &check
	at.SignIn = &router.SignIn{Args: []string{"auth", "login"}, CredentialEnv: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}}
	types["claude-code"] = at
}

// signInHarness: claude-code installed on jet01 and signed out.
func signInHarness(t *testing.T, opts ...router.Option) *availabilityHarness {
	t.Helper()
	h := newAvailabilityHarnessOpts(t, withSignIn, opts...)
	h.probes.install("claude")
	h.probes.setAuth("claude", claudeSignedOut)
	return h
}

func (h *availabilityHarness) prepare(t *testing.T, restart bool) *router.SignInResult {
	t.Helper()
	res, err := h.r.PrepareSignIn(context.Background(), h.target.ID, "claude-code", restart)
	if err != nil {
		t.Fatalf("PrepareSignIn: %v", err)
	}
	return res
}

// LOOM-316: a signed-out agent gets its own login command opened in a
// tmux session for a person to attach to, run through the probed path,
// and the session is recorded. Loomux types nothing into it.
func TestPrepareSignIn_OpensTheCLIsLogin(t *testing.T) {
	h := signInHarness(t)
	res := h.prepare(t, false)
	if !res.Opened || res.Reused {
		t.Fatalf("result = %+v, want a session opened", res)
	}
	s := h.exec.sessionFor(signInSession)
	if s == nil || !s.alive {
		t.Fatalf("no live %s session", signInSession)
	}
	for _, want := range []string{"'" + fakeBinDir + "claude' 'auth' 'login'", `exec "${SHELL:-/bin/sh}"`} {
		if !strings.Contains(s.command, want) {
			t.Errorf("session command missing %q:\n%s", want, s.command)
		}
	}
	if s.dir != "" {
		t.Errorf("session dir = %q, want the user's home (empty)", s.dir)
	}
	if len(s.keys) != 0 || len(s.namedKeys) != 0 {
		t.Errorf("keys sent to the sign-in session: %q %q", s.keys, s.namedKeys)
	}
	rec := h.agentRecord(t, "claude-code")
	if rec.SignInSession != signInSession || rec.SignInStartedAt == nil || time.Since(*rec.SignInStartedAt) > time.Minute {
		t.Errorf("recorded session = %q since %v", rec.SignInSession, rec.SignInStartedAt)
	}
	if res.Agent == nil || res.Agent.SignInSession != signInSession || res.Agent.AuthStatus != registry.AgentAuthLoggedOut {
		t.Errorf("result agent = %+v, want the recorded row", res.Agent)
	}
}

// A signed-in agent needs nothing opened.
func TestPrepareSignIn_SignedInOpensNothing(t *testing.T) {
	h := signInHarness(t)
	h.probes.setAuth("claude", claudeSignedIn)
	res := h.prepare(t, false)
	if res.Opened || res.Reused {
		t.Fatalf("result = %+v, want nothing opened", res)
	}
	if s := h.exec.sessionFor(signInSession); s != nil {
		t.Errorf("a sign-in session was opened for a signed-in agent: %+v", s)
	}
	if res.Agent.AuthStatus != registry.AgentAuthLoggedIn || res.Agent.AuthMethod != "claude.ai" || res.Agent.AuthPlan != "max" {
		t.Errorf("agent = %+v, want logged in via claude.ai on max", res.Agent)
	}
}

// A session already open is handed back, not replaced: a half-done login
// is never killed by a second caller. restart replaces it.
func TestPrepareSignIn_ReusesTheOpenSessionUnlessRestarted(t *testing.T) {
	h := signInHarness(t)
	h.prepare(t, false)
	first := h.exec.sessionFor(signInSession)

	res := h.prepare(t, false)
	if res.Opened || !res.Reused {
		t.Fatalf("second call = %+v, want the open session reused", res)
	}
	if h.exec.sessionFor(signInSession) != first || !first.alive {
		t.Error("the open session was replaced")
	}

	res = h.prepare(t, true)
	if !res.Opened || res.Reused {
		t.Fatalf("restart = %+v, want a fresh session", res)
	}
	if again := h.exec.sessionFor(signInSession); again == first || !again.alive {
		t.Error("restart didn't open a fresh session")
	}
}

// A recorded session that is gone (the person exited its shell, the
// orphan sweep took it) is opened again.
func TestPrepareSignIn_ReopensASessionThatIsGone(t *testing.T) {
	h := signInHarness(t)
	h.prepare(t, false)
	if err := h.exec.KillSession(context.Background(), signInSession); err != nil {
		t.Fatal(err)
	}
	if res := h.prepare(t, false); !res.Opened {
		t.Fatalf("result = %+v, want it opened again", res)
	}
	if s := h.exec.sessionFor(signInSession); s == nil || !s.alive {
		t.Error("no live sign-in session")
	}
}

func TestPrepareSignIn_Refusals(t *testing.T) {
	h := signInHarness(t)
	ctx := context.Background()
	if _, err := h.r.PrepareSignIn(ctx, h.target.ID, "nope", false); !errors.Is(err, router.ErrUnknownAgentType) {
		t.Errorf("unknown agent type: err = %v, want ErrUnknownAgentType", err)
	}
	if _, err := h.r.PrepareSignIn(ctx, h.target.ID, "codex", false); !errors.Is(err, router.ErrNoSignIn) {
		t.Errorf("agent type without a sign-in: err = %v, want ErrNoSignIn", err)
	}
	if _, err := h.r.PrepareSignIn(ctx, uuid.NewString(), "claude-code", false); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("unknown target: err = %v, want ErrNotFound", err)
	}
	h.probes.mu.Lock()
	h.probes.installed["claude"] = false
	h.probes.mu.Unlock()
	var unavailable *router.AgentUnavailableError
	if _, err := h.r.PrepareSignIn(ctx, h.target.ID, "claude-code", false); !errors.As(err, &unavailable) {
		t.Errorf("not installed: err = %v, want AgentUnavailableError", err)
	}
	if s := h.exec.sessionFor(signInSession); s != nil {
		t.Errorf("a refused sign-in opened a session: %+v", s)
	}
}

// LOOM-316: a probe that finds the agent signed in after it was signed
// out closes the sign-in session: the person is done.
func TestProbe_SignedInClosesTheSignInSession(t *testing.T) {
	h := signInHarness(t)
	h.prepare(t, false)
	h.probes.setAuth("claude", claudeSignedIn)
	if _, err := h.r.RefreshTargetAgents(context.Background(), h.target.ID); err != nil {
		t.Fatalf("RefreshTargetAgents: %v", err)
	}
	if s := h.exec.sessionFor(signInSession); s == nil || s.alive {
		t.Error("the sign-in session is still open after the agent signed in")
	}
	rec := h.agentRecord(t, "claude-code")
	if rec.SignInSession != "" || rec.SignInStartedAt != nil || rec.AuthStatus != registry.AgentAuthLoggedIn {
		t.Errorf("record = %+v, want signed in with no session", rec)
	}
}

// While still signed out, a probe leaves the session to the person.
func TestProbe_SignedOutKeepsTheSignInSession(t *testing.T) {
	h := signInHarness(t)
	h.prepare(t, false)
	if _, err := h.r.RefreshTargetAgents(context.Background(), h.target.ID); err != nil {
		t.Fatalf("RefreshTargetAgents: %v", err)
	}
	if s := h.exec.sessionFor(signInSession); s == nil || !s.alive {
		t.Error("a probe closed the sign-in session of a still signed-out agent")
	}
}

func TestCloseSignIn(t *testing.T) {
	h := signInHarness(t)
	ctx := context.Background()
	if err := h.r.CloseSignIn(ctx, h.target.ID, "claude-code"); err != nil {
		t.Fatalf("CloseSignIn with nothing open: %v", err)
	}
	h.prepare(t, false)
	if err := h.r.CloseSignIn(ctx, h.target.ID, "claude-code"); err != nil {
		t.Fatalf("CloseSignIn: %v", err)
	}
	if s := h.exec.sessionFor(signInSession); s == nil || s.alive {
		t.Error("the sign-in session is still open")
	}
	if rec := h.agentRecord(t, "claude-code"); rec.SignInSession != "" {
		t.Errorf("record still names %q", rec.SignInSession)
	}
	if err := h.r.CloseSignIn(ctx, h.target.ID, "nope"); !errors.Is(err, router.ErrUnknownAgentType) {
		t.Errorf("unknown agent type: err = %v, want ErrUnknownAgentType", err)
	}
}

// The method and plan are kept only as short words: anything else an
// adapter's parser lets through (an email, a sentence) is dropped.
func TestProbe_AuthDetailsAreShortWordsOnly(t *testing.T) {
	h := signInHarness(t)
	cases := []struct{ out, method, plan string }{
		{`{"loggedIn": true, "authMethod": "Claude.AI", "subscriptionType": "max"}`, "claude.ai", "max"},
		{`{"loggedIn": true, "authMethod": "someone@example.com", "subscriptionType": "Max plan for someone"}`, "", ""},
		{`{"loggedIn": true, "authMethod": "` + strings.Repeat("a", 40) + `"}`, "", ""},
	}
	for _, tc := range cases {
		h.probes.setAuth("claude", tc.out)
		if _, err := h.r.RefreshTargetAgents(context.Background(), h.target.ID); err != nil {
			t.Fatalf("RefreshTargetAgents: %v", err)
		}
		rec := h.agentRecord(t, "claude-code")
		if rec.AuthMethod != tc.method || rec.AuthPlan != tc.plan {
			t.Errorf("auth output %s: method %q plan %q, want %q %q", tc.out, rec.AuthMethod, rec.AuthPlan, tc.method, tc.plan)
		}
	}
}

// LOOM-316, the dispatch gate: the probe right before a launch says the
// agent is signed out, so instead of an agent that can only stop at its
// sign-in screen, the dispatch fails at once with login_required naming
// the sign-in session to attach to. No agent task is launched.
func TestDispatch_SignedOutAgentOpensItsSignInInsteadOfLaunching(t *testing.T) {
	h := signInHarness(t)
	h.decide(h.provisionDecision("claude-code"))
	_, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01")
	if err == nil {
		t.Fatal("Dispatch: nil error for a signed-out agent")
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassLoginRequired {
		t.Errorf("ClassifyError = %q, want login_required", got)
	}
	for _, want := range []string{"isn't signed in", "jet01", "tmux -L loomux attach -t " + signInSession, "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	tasks, _ := h.store.ListTasks(context.Background())
	for _, task := range tasks {
		if task.Kind == registry.TaskKindAgent {
			t.Errorf("an agent task was launched: %+v", task)
		}
	}
	if s := h.exec.sessionFor(signInSession); s == nil || !s.alive {
		t.Error("no sign-in session opened")
	}
}

// An API key for the agent in the vault signs it in without a login: the
// gate steps aside and the agent launches.
func TestDispatch_VaultKeyBypassesTheSignInGate(t *testing.T) {
	h := signInHarness(t)
	if err := h.store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "ANTHROPIC_API_KEY", AgentType: "claude-code", Value: "sk-ant-test-0000000000",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	h.decide(h.provisionDecision("claude-code"))
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if s := h.exec.sessionFor(signInSession); s != nil {
		t.Errorf("a sign-in session was opened despite the vault key: %+v", s)
	}
	launched := false
	tasks, _ := h.store.ListTasks(context.Background())
	for _, task := range tasks {
		launched = launched || task.Kind == registry.TaskKindAgent
	}
	if !launched {
		t.Error("the agent wasn't launched")
	}
}

// A credential for something else (a GitHub token) is no sign-in.
func TestDispatch_OtherVaultCredentialsDontBypassTheGate(t *testing.T) {
	h := signInHarness(t)
	if err := h.store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "GITHUB_TOKEN", Value: "ghp-test-000000000000",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	h.decide(h.provisionDecision("claude-code"))
	_, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01")
	if got := router.ClassifyError(err); got != registry.ErrorClassLoginRequired {
		t.Fatalf("ClassifyError = %q (err %v), want login_required", got, err)
	}
}

// signedInAttentionHarness: the attention harness's claude-code made a
// probed CLI with a sign-in, signed in as far as its own check says.
func signedInAttentionHarness(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *registry.Workspace, *probeScript) {
	t.Helper()
	probes := &probeScript{installed: map[string]bool{"claude": true}, auth: map[string]string{"claude": claudeSignedIn}}
	store, exec, r, ws := attentionHarnessWith(t, func(exec *fakeExecutor, types router.AgentTypeRegistry) {
		exec.runOnce = probes.runOnce
		at := types["claude-code"]
		at.Binary = "claude"
		at.AuthCheck = &router.AuthCheck{Args: []string{"auth", "status"},
			LoggedIn: regexp.MustCompile(`"loggedIn":\s*true`), LoggedOut: regexp.MustCompile(`"loggedIn":\s*false`)}
		types["claude-code"] = at
		withSignIn(types)
	})
	return store, exec, r, ws, probes
}

func agentRow(t *testing.T, store registry.Store, ws *registry.Workspace) *registry.TargetAgent {
	t.Helper()
	rec, err := store.GetTargetAgent(context.Background(), ws.TargetID, "claude-code")
	if err != nil {
		t.Fatalf("GetTargetAgent: %v", err)
	}
	return rec
}

// LOOM-316: a turn that still reaches a sign-in screen (the CLI's own
// check said signed in) records what it saw, opens the sign-in session
// and names it first; the task's own pane is kept as before.
func TestDispatch_SignInScreenRecordsTheProblemAndOpensASignIn(t *testing.T) {
	store, exec, r, ws, _ := signedInAttentionHarness(t)
	exec.capture = "LOGIN"
	_, err := r.Dispatch(context.Background(), "conv-1", "hello")
	if got := router.ClassifyError(err); got != registry.ErrorClassLoginRequired {
		t.Fatalf("ClassifyError = %q (err %v), want login_required", got, err)
	}
	task := onlyTask(t, store, ws.ID)
	msg := err.Error()
	signIn := strings.Index(msg, "tmux -L loomux attach -t "+signInSession)
	pane := strings.Index(msg, "tmux -L loomux attach -t "+task.TmuxSession)
	if signIn < 0 || pane < 0 || signIn > pane {
		t.Errorf("error should name the sign-in session first, then the task's pane: %v", err)
	}
	if s := exec.sessionFor(signInSession); s == nil || !s.alive {
		t.Error("no sign-in session opened")
	}
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the task's pane was torn down")
	}
	rec := agentRow(t, store, ws)
	if rec.SignInProblem != registry.AgentSignInSignedOut || rec.SignInProblemAt == nil {
		t.Errorf("sign-in problem = %q at %v, want signed_out recorded", rec.SignInProblem, rec.SignInProblemAt)
	}

	// The person signs in; the next turn finishes normally, which proves
	// it: the problem is cleared and the sign-in session closed.
	exec.set(func() {
		exec.capture = "all done"
		exec.fileExists = true
	})
	if _, err := r.Dispatch(context.Background(), "conv-2", "hello again"); err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	rec = agentRow(t, store, ws)
	if rec.SignInProblem != "" || rec.SignInSession != "" {
		t.Errorf("after a finished turn: problem %q, session %q; want both cleared", rec.SignInProblem, rec.SignInSession)
	}
	if s := exec.sessionFor(signInSession); s == nil || s.alive {
		t.Error("the sign-in session is still open after a finished turn")
	}
}

// LOOM-316: a usage limit is recorded against the agent on that target
// until a later turn there finishes normally.
func TestDispatch_UsageLimitIsRecordedUntilATurnFinishes(t *testing.T) {
	store, exec, r, ws, _ := signedInAttentionHarness(t)
	exec.set(func() {
		exec.capture = "LIMIT"
		exec.fileExists = true
	})
	if _, err := r.Dispatch(context.Background(), "conv-1", "hello"); router.ClassifyError(err) != registry.ErrorClassAgentRateLimited {
		t.Fatalf("Dispatch: err %v, want agent_rate_limited", err)
	}
	rec := agentRow(t, store, ws)
	if rec.LimitedAt == nil || rec.LimitResets != "5pm (Europe/Istanbul)" {
		t.Fatalf("rate limit = %v %q, want it recorded with its reset", rec.LimitedAt, rec.LimitResets)
	}

	exec.set(func() { exec.capture = "all done" })
	if _, err := r.Dispatch(context.Background(), "conv-2", "hello again"); err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if rec = agentRow(t, store, ws); rec.LimitedAt != nil || rec.LimitResets != "" {
		t.Errorf("rate limit = %v %q after a finished turn, want cleared", rec.LimitedAt, rec.LimitResets)
	}
}

func TestSignInProblemFor(t *testing.T) {
	cases := map[string]string{
		"OAuth token has expired. Please obtain a new token or refresh your existing token.": registry.AgentSignInExpired,
		"Invalid API key · Please run /login":                                               registry.AgentSignInInvalid,
		"Select login method:":                                                              registry.AgentSignInSignedOut,
		"Sign in with ChatGPT":                                                              registry.AgentSignInSignedOut,
		"Please run /login":                                                                 registry.AgentSignInSignedOut,
	}
	for detail, want := range cases {
		if got := router.SignInProblemFor(detail); got != want {
			t.Errorf("SignInProblemFor(%q) = %q, want %q", detail, got, want)
		}
	}
}
