package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// fakeSignIns stands in for the router's prepared sign-in (LOOM-316).
type fakeSignIns struct {
	mu       sync.Mutex
	store    registry.Store
	err      error
	opened   bool
	prepares []string
	closes   []string
}

func (f *fakeSignIns) PrepareAgentSignIn(ctx context.Context, targetID, agentType string, restart bool) (*api.AgentSignInResult, error) {
	f.mu.Lock()
	f.prepares = append(f.prepares, fmt.Sprintf("%s/%s restart=%v", targetID, agentType, restart))
	err, opened := f.err, f.opened
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if opened {
		if err := f.store.SetAgentSignInSession(ctx, targetID, agentType, "loomux-signin-"+agentType, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	rec, err := f.store.GetTargetAgent(ctx, targetID, agentType)
	if err != nil {
		return nil, err
	}
	return &api.AgentSignInResult{Agent: rec, Opened: opened}, nil
}

func (f *fakeSignIns) CloseAgentSignIn(ctx context.Context, targetID, agentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, targetID+"/"+agentType)
	return f.err
}

type agentAuthJSON struct {
	State     string     `json:"state"`
	Method    string     `json:"method"`
	Plan      string     `json:"plan"`
	ExpiresAt *time.Time `json:"expires_at"`
	Problem   *struct {
		Kind       string    `json:"kind"`
		ObservedAt time.Time `json:"observed_at"`
	} `json:"problem"`
	RateLimit *struct {
		ObservedAt time.Time `json:"observed_at"`
		Resets     string    `json:"resets"`
	} `json:"rate_limit"`
	SignIn *struct {
		TmuxSession    string    `json:"tmux_session"`
		StartedAt      time.Time `json:"started_at"`
		AttachCommand  string    `json:"attach_command"`
		AttachCommands []struct {
			Via     string   `json:"via"`
			Command string   `json:"command"`
			Argv    []string `json:"argv"`
		} `json:"attach_commands"`
	} `json:"sign_in"`
}

type agentWithAuthJSON struct {
	AgentType  string         `json:"agent_type"`
	AuthStatus string         `json:"auth_status"`
	Auth       *agentAuthJSON `json:"auth"`
}

func signInServer(t *testing.T) (string, string, registry.Store, *fakeSignIns, *registry.Target) {
	t.Helper()
	signIns := &fakeSignIns{}
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		signIns.store = s
		return []api.Option{api.WithAgentSignIns(signIns)}
	})
	token, _ := login(t, srv.URL, testPassword)
	target := &registry.Target{ID: uuid.NewString(), Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example", User: "dev"}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	return srv.URL, token, store, signIns, target
}

func getAgents(t *testing.T, url, token, targetID string) []agentWithAuthJSON {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, url+"/api/v1/targets/"+targetID+"/agents", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Agents []agentWithAuthJSON `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Agents
}

// LOOM-316: each probed agent's sign-in, as the one word to show and
// what is behind it — method, plan, expiry, a usage limit, the open
// sign-in session and how to attach to it.
func TestListTargetAgents_Auth(t *testing.T) {
	url, token, store, _, target := signInServer(t)
	ctx := context.Background()
	expires := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	if err := store.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code", Available: true,
		Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedIn, AuthMethod: "claude.ai", AuthPlan: "max",
		AuthExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	limited := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err := store.SetAgentRateLimit(ctx, target.ID, "claude-code", limited, "5pm (Europe/Berlin)"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentSignInSession(ctx, target.ID, "claude-code", "loomux-signin-claude-code", limited); err != nil {
		t.Fatal(err)
	}
	// An agent type with no auth check: no auth block.
	if err := store.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "shell-agent", Available: true,
		Path: "/usr/bin/sh"}); err != nil {
		t.Fatal(err)
	}

	agents := getAgents(t, url, token, target.ID)
	if len(agents) != 2 {
		t.Fatalf("agents = %+v, want two", agents)
	}
	claude := agents[0]
	if claude.AuthStatus != registry.AgentAuthLoggedIn || claude.Auth == nil {
		t.Fatalf("claude-code = %+v, want auth_status logged_in and an auth block", claude)
	}
	a := claude.Auth
	if a.State != "rate_limited" || a.Method != "claude.ai" || a.Plan != "max" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(expires) {
		t.Errorf("auth = %+v, want rate_limited via claude.ai on max, expiring %v", a, expires)
	}
	if a.RateLimit == nil || a.RateLimit.Resets != "5pm (Europe/Berlin)" || !a.RateLimit.ObservedAt.Equal(limited) {
		t.Errorf("rate_limit = %+v", a.RateLimit)
	}
	if a.Problem != nil {
		t.Errorf("problem = %+v, want none", a.Problem)
	}
	if a.SignIn == nil || a.SignIn.TmuxSession != "loomux-signin-claude-code" ||
		a.SignIn.AttachCommand != targets.AttachCommand(targets.DefaultTmuxSocket, "loomux-signin-claude-code") {
		t.Fatalf("sign_in = %+v", a.SignIn)
	}
	if len(a.SignIn.AttachCommands) != 1 || a.SignIn.AttachCommands[0].Via != "ssh" ||
		!strings.Contains(a.SignIn.AttachCommands[0].Command, "dev@jet01.example") ||
		!strings.Contains(a.SignIn.AttachCommands[0].Command, "loomux-signin-claude-code") {
		t.Errorf("attach_commands = %+v, want the ssh form", a.SignIn.AttachCommands)
	}
	if agents[1].Auth != nil {
		t.Errorf("an agent with no auth check has an auth block: %+v", agents[1].Auth)
	}
}

// A sign-in problem a turn saw shows with its kind and time; a sign-in
// session past the orphan sweep's age isn't reported.
func TestListTargetAgents_AuthProblemAndStaleSession(t *testing.T) {
	url, token, store, _, target := signInServer(t)
	ctx := context.Background()
	if err := store.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "codex", Available: true,
		Path: "/usr/bin/codex", AuthStatus: registry.AgentAuthLoggedIn, AuthMethod: "chatgpt"}); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := store.SetAgentSignInProblem(ctx, target.ID, "codex", registry.AgentSignInExpired, seen); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentSignInSession(ctx, target.ID, "codex", "loomux-signin-codex",
		time.Now().Add(-registry.SignInSessionTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}
	a := getAgents(t, url, token, target.ID)[0].Auth
	if a == nil || a.State != "expired" || a.Problem == nil || a.Problem.Kind != "expired" || !a.Problem.ObservedAt.Equal(seen) {
		t.Fatalf("auth = %+v, want expired with the problem", a)
	}
	if a.SignIn != nil {
		t.Errorf("a sign-in session past its TTL is reported: %+v", a.SignIn)
	}
}

func signInRequest(t *testing.T, method, url, token, targetID, agentType, body string) *http.Response {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	return authedRequest(t, method, url+"/api/v1/targets/"+targetID+"/agents/"+agentType+"/sign-in", token, b)
}

// LOOM-316: POST …/sign-in opens the agent's sign-in and returns the
// agent with how to attach.
func TestAgentSignIn_Opens(t *testing.T) {
	url, token, store, signIns, target := signInServer(t)
	if err := store.SetTargetAgent(context.Background(), &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code",
		Available: true, Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedOut}); err != nil {
		t.Fatal(err)
	}
	signIns.opened = true
	resp := signInRequest(t, http.MethodPost, url, token, target.ID, "claude-code", `{"restart": true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Agent  agentWithAuthJSON `json:"agent"`
		Opened bool              `json:"opened"`
		Reused bool              `json:"reused"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Opened || out.Reused || out.Agent.AgentType != "claude-code" || out.Agent.Auth == nil ||
		out.Agent.Auth.State != "logged_out" || out.Agent.Auth.SignIn == nil ||
		out.Agent.Auth.SignIn.AttachCommand != "tmux -L loomux attach -t loomux-signin-claude-code" {
		t.Errorf("response = %+v", out)
	}
	if want := target.ID + "/claude-code restart=true"; len(signIns.prepares) != 1 || signIns.prepares[0] != want {
		t.Errorf("prepares = %q, want [%q]", signIns.prepares, want)
	}

	// No body is restart false.
	resp2 := signInRequest(t, http.MethodPost, url, token, target.ID, "claude-code", "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || signIns.prepares[1] != target.ID+"/claude-code restart=false" {
		t.Errorf("no body: status %d, prepares %q", resp2.StatusCode, signIns.prepares)
	}
}

func TestAgentSignIn_Errors(t *testing.T) {
	url, token, _, signIns, target := signInServer(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{api.ErrUnknownAgentType, http.StatusNotFound, "not_found"},
		{fmt.Errorf("x: %w", registry.ErrNotFound), http.StatusNotFound, "not_found"},
		{api.ErrAgentNotInstalled, http.StatusConflict, "agent_unavailable"},
		{api.ErrNoAgentSignIn, http.StatusUnprocessableEntity, "no_sign_in"},
		{fmt.Errorf("probe: %w", targets.ErrUnreachable), http.StatusBadGateway, "target_unreachable"},
		{errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		signIns.mu.Lock()
		signIns.err = tc.err
		signIns.mu.Unlock()
		resp := signInRequest(t, http.MethodPost, url, token, target.ID, "claude-code", "")
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || body.Code != tc.code {
			t.Errorf("%v: status %d code %q, want %d %q", tc.err, resp.StatusCode, body.Code, tc.status, tc.code)
		}
		if strings.Contains(body.Error, "boom") {
			t.Errorf("an internal error's text reached the client: %q", body.Error)
		}
	}
	bad := signInRequest(t, http.MethodPost, url, token, target.ID, "claude-code", `{"restart": "yes"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", bad.StatusCode)
	}
}

func TestAgentSignIn_Close(t *testing.T) {
	url, token, _, signIns, target := signInServer(t)
	resp := signInRequest(t, http.MethodDelete, url, token, target.ID, "codex", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(signIns.closes) != 1 || signIns.closes[0] != target.ID+"/codex" {
		t.Errorf("closes = %q", signIns.closes)
	}
	signIns.err = api.ErrUnknownAgentType
	resp = signInRequest(t, http.MethodDelete, url, token, target.ID, "nope", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent type: status %d, want 404", resp.StatusCode)
	}
}

// Without a sign-in backend the routes say so.
func TestAgentSignIn_NotConfigured(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	target := &registry.Target{ID: "t1", Name: "jet01", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	resp := signInRequest(t, http.MethodPost, srv.URL, token, "t1", "codex", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
