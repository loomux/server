package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAgentAPI answers the routes `loomuxd agents` uses (LOOM-316) with
// two targets: jet01, whose claude-code is signed out with a sign-in
// open, and sc1, whose codex is signed in.
type fakeAgentAPI struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

const jet01ClaudeOpen = `{"agent_type":"claude-code","available":true,"version":"2.1.329","auth_status":"logged_out",` +
	`"checked_at":"2026-11-20T10:00:00Z","auth":{"state":"logged_out","method":"none","sign_in":{` +
	`"tmux_session":"loomux-signin-claude-code","started_at":"2026-11-20T10:00:00Z",` +
	`"attach_command":"tmux -L loomux attach -t loomux-signin-claude-code",` +
	`"attach_commands":[{"via":"ssh","command":"ssh dev@jet01 -t 'tmux -L loomux attach -t loomux-signin-claude-code'"}]}}}`

func (f *fakeAgentAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
		}
	}
	reply := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("GET /api/v1/targets", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"targets":[{"id":"t-jet","name":"jet01"},{"id":"t-sc1","name":"sc1"}]}`)
	})
	mux.HandleFunc("GET /api/v1/targets/t-jet/agents", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"agents":[`+jet01ClaudeOpen+`,{"agent_type":"codex","available":false,"checked_at":"2026-11-20T10:00:00Z"}]}`)
	})
	mux.HandleFunc("GET /api/v1/targets/t-sc1/agents", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"agents":[{"agent_type":"codex","available":true,"auth_status":"logged_in","checked_at":"2026-11-20T09:00:00Z",`+
			`"auth":{"state":"logged_in","method":"chatgpt"}}]}`)
	})
	mux.HandleFunc("POST /api/v1/targets/t-sc1/probe", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"health":{"reachable":true},"agents":[{"agent_type":"codex","available":true,"auth_status":"logged_in",`+
			`"checked_at":"2026-11-20T11:00:00Z","auth":{"state":"rate_limited","method":"chatgpt","rate_limit":`+
			`{"observed_at":"2026-11-20T10:30:00Z","resets":"in 2 hours"}}}]}`)
	})
	mux.HandleFunc("POST /api/v1/targets/t-jet/agents/claude-code/sign-in", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"agent":`+jet01ClaudeOpen+`,"opened":true,"reused":false}`)
	})
	mux.HandleFunc("POST /api/v1/targets/t-sc1/agents/codex/sign-in", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 200, `{"agent":{"agent_type":"codex","available":true,"auth_status":"logged_in",`+
			`"auth":{"state":"logged_in","method":"chatgpt"}},"opened":false,"reused":false}`)
	})
	mux.HandleFunc("POST /api/v1/targets/t-jet/agents/codex/sign-in", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, 409, `{"error":"the agent's CLI isn't installed on this target","code":"agent_unavailable"}`)
	})
	mux.HandleFunc("DELETE /api/v1/targets/t-jet/agents/claude-code/sign-in", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func runAgentsAgainst(t *testing.T, args ...string) (stdout, stderr string, code int, api *fakeAgentAPI) {
	t.Helper()
	api = &fakeAgentAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	env := map[string]string{"LOOMUX_URL": srv.URL + "/", "LOOMUX_TOKEN": "tok"}
	var out, errOut bytes.Buffer
	code = runAgents(args, func(k string) string { return env[k] }, &out, &errOut)
	return out.String(), errOut.String(), code, api
}

func TestAgentsStatus(t *testing.T) {
	out, errOut, code, _ := runAgentsAgainst(t, "status")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	for _, want := range []string{
		"TARGET", "AGENT", "STATE",
		"jet01", "claude-code", "logged_out",
		"not installed",
		"sc1", "codex", "logged_in", "chatgpt",
		"ssh dev@jet01 -t 'tmux -L loomux attach -t loomux-signin-claude-code'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestAgentsStatus_OneTargetRefreshed(t *testing.T) {
	out, errOut, code, api := runAgentsAgainst(t, "status", "-target", "sc1", "-refresh")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if strings.Contains(out, "jet01") {
		t.Errorf("-target sc1 listed jet01:\n%s", out)
	}
	for _, want := range []string{"rate_limited", "resets in 2 hours"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	want := []string{"GET /api/v1/targets", "POST /api/v1/targets/t-sc1/probe"}
	if strings.Join(api.requests, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %q, want %q", api.requests, want)
	}
}

func TestAgentsSignIn_Opened(t *testing.T) {
	out, errOut, code, api := runAgentsAgainst(t, "sign-in", "-target", "jet01", "-agent", "claude-code", "-restart")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	for _, want := range []string{"claude-code", "jet01", "ssh dev@jet01 -t 'tmux -L loomux attach -t loomux-signin-claude-code'",
		"tmux -L loomux attach -t loomux-signin-claude-code"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	var body struct {
		Restart bool `json:"restart"`
	}
	if err := json.Unmarshal([]byte(api.bodies[len(api.bodies)-1]), &body); err != nil || !body.Restart {
		t.Errorf("request body = %q, want restart true", api.bodies[len(api.bodies)-1])
	}
}

func TestAgentsSignIn_AlreadySignedIn(t *testing.T) {
	out, _, code, _ := runAgentsAgainst(t, "sign-in", "-target", "t-sc1", "-agent", "codex")
	if code != 0 || !strings.Contains(out, "already signed in") {
		t.Errorf("exit %d, output %q; want it to say codex is already signed in", code, out)
	}
}

func TestAgentsSignIn_APIError(t *testing.T) {
	_, errOut, code, _ := runAgentsAgainst(t, "sign-in", "-target", "jet01", "-agent", "codex")
	if code == 0 || !strings.Contains(errOut, "isn't installed") || !strings.Contains(errOut, "agent_unavailable") {
		t.Errorf("exit %d, stderr %q; want the API's error", code, errOut)
	}
}

func TestAgentsSignIn_Close(t *testing.T) {
	out, errOut, code, api := runAgentsAgainst(t, "sign-in", "-close", "-target", "jet01", "-agent", "claude-code")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if api.requests[len(api.requests)-1] != "DELETE /api/v1/targets/t-jet/agents/claude-code/sign-in" || !strings.Contains(out, "closed") {
		t.Errorf("requests %q, output %q", api.requests, out)
	}
}

func TestAgents_Usage(t *testing.T) {
	cases := [][]string{
		{},
		{"nope"},
		{"sign-in", "-target", "jet01"},
		{"sign-in", "-agent", "codex"},
		{"status", "-target", "nowhere"},
	}
	for _, args := range cases {
		if _, _, code, _ := runAgentsAgainst(t, args...); code == 0 {
			t.Errorf("loomuxd agents %q: exit 0, want an error", args)
		}
	}
	var out, errOut bytes.Buffer
	code := runAgents([]string{"status"}, func(string) string { return "" }, &out, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "LOOMUX_TOKEN") {
		t.Errorf("no token: exit %d, stderr %q; want it to ask for LOOMUX_TOKEN", code, errOut.String())
	}
}
