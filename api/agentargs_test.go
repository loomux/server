package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// fakeAgentArgsCheck stands in for router.AgentTypeRegistry.CheckAgentArgs:
// claude-code takes low and high, codex takes a model only.
func fakeAgentArgsCheck(agentType string, a registry.AgentArgs) error {
	switch agentType {
	case "claude-code":
		if a.Effort != "" && !slices.Contains([]string{"low", "high"}, a.Effort) {
			return fmt.Errorf("effort %q is not one claude-code takes (low, high)", a.Effort)
		}
	case "codex":
		if a.Effort != "" {
			return fmt.Errorf("codex takes no effort")
		}
	default:
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	return nil
}

type agentArgsTargetJSON struct {
	ID        string                       `json:"id"`
	AgentArgs map[string]map[string]string `json:"agent_args"`
}

func decodeAgentArgsTarget(t *testing.T, body string) agentArgsTargetJSON {
	t.Helper()
	var out agentArgsTargetJSON
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

// LOOM-307: agent_args are set through the target API, shown on every
// read with both fields, and a PUT replaces the whole map only when it
// names agent_args.
func TestTargetAgentArgs_SetShowReplace(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithAgentArgsCheck(fakeAgentArgsCheck))
	token, _ := login(t, srv.URL, testPassword)
	ctx := context.Background()

	status, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/targets", token, map[string]any{
		"name": "sc1", "kind": "remote", "host": "sc1.example", "user": "u",
		"agent_args": map[string]any{"claude-code": map[string]string{"model": "opus", "effort": "high"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("POST = %d %s", status, body)
	}
	created := decodeAgentArgsTarget(t, body)
	want := map[string]map[string]string{"claude-code": {"model": "opus", "effort": "high"}}
	if !reflect.DeepEqual(created.AgentArgs, want) {
		t.Errorf("POST agent_args = %v, want %v", created.AgentArgs, want)
	}
	stored, _ := store.GetTarget(ctx, created.ID)
	if got := stored.AgentArgs["claude-code"]; got != (registry.AgentArgs{Model: "opus", Effort: "high"}) {
		t.Errorf("stored = %+v", stored.AgentArgs)
	}

	put := func(req map[string]any) agentArgsTargetJSON {
		t.Helper()
		status, body := doJSON(t, http.MethodPut, srv.URL+"/api/v1/targets/"+created.ID, token, req)
		if status != http.StatusOK {
			t.Fatalf("PUT %v = %d %s", req, status, body)
		}
		return decodeAgentArgsTarget(t, body)
	}
	if got := put(map[string]any{"permission_mode": "manual"}); !reflect.DeepEqual(got.AgentArgs, want) {
		t.Errorf("PUT without agent_args: agent_args = %v, want them kept (%v)", got.AgentArgs, want)
	}
	// Named: the whole map is replaced; an entry pinning nothing is dropped.
	got := put(map[string]any{"agent_args": map[string]any{
		"codex": map[string]string{"model": "gpt-5.1-codex"}, "claude-code": map[string]string{"model": "", "effort": ""},
	}})
	want = map[string]map[string]string{"codex": {"model": "gpt-5.1-codex", "effort": ""}}
	if !reflect.DeepEqual(got.AgentArgs, want) {
		t.Errorf("PUT agent_args = %v, want %v", got.AgentArgs, want)
	}
	if got := put(map[string]any{"agent_args": map[string]any{}}); len(got.AgentArgs) != 0 || got.AgentArgs == nil {
		t.Errorf("PUT agent_args {} = %v, want {} (none, and never null)", got.AgentArgs)
	}
	status, body = doJSON(t, http.MethodGet, srv.URL+"/api/v1/targets/"+created.ID, token, nil)
	if status != http.StatusOK || !strings.Contains(body, `"agent_args":{}`) {
		t.Errorf("GET = %d %s, want agent_args {}", status, body)
	}
}

// Strict validation: only registered agent types, only their efforts,
// only a model name; a plugin machine's request is checked the same way.
func TestTargetAgentArgs_Refused(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithAgentArgsCheck(fakeAgentArgsCheck))
	token, _ := login(t, srv.URL, testPassword)
	for _, tc := range []struct {
		name    string
		args    any
		plugin  bool
		wantMsg string
	}{
		{"unknown agent type", map[string]any{"gemini": map[string]string{"model": "pro"}}, false,
			`agent_args["gemini"]: unknown agent type "gemini"`},
		{"effort not offered", map[string]any{"claude-code": map[string]string{"effort": "max"}}, false,
			`agent_args["claude-code"]: effort "max" is not one claude-code takes (low, high)`},
		{"model with a flag", map[string]any{"claude-code": map[string]string{"model": "opus --dangerously-skip-permissions"}}, false,
			`agent_args["claude-code"].model: ` + registry.AgentModelRule},
		{"model with a substitution", map[string]any{"codex": map[string]string{"model": "$(id)"}}, false,
			`agent_args["codex"].model: ` + registry.AgentModelRule},
		{"unknown field", map[string]any{"codex": map[string]string{"model": "o3", "args": "--yolo"}}, false, ""},
		{"not an object", map[string]any{"codex": "-m o3"}, false, ""},
		{"plugin machine", map[string]any{"claude-code": map[string]string{"effort": "max"}}, true,
			`agent_args["claude-code"]: effort "max" is not one claude-code takes (low, high)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{"name": "box", "kind": "remote", "host": "box", "user": "u", "agent_args": tc.args}
			if tc.plugin {
				req = map[string]any{"name": "box", "plugin": map[string]string{"id": "docker"}, "agent_args": tc.args}
			}
			status, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/targets", token, req)
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal([]byte(body), &e)
			if status != http.StatusBadRequest || !strings.Contains(e.Error, tc.wantMsg) {
				t.Errorf("POST = %d %s, want 400 with %q", status, body, tc.wantMsg)
			}
		})
	}
	if targets, _ := store.ListTargets(context.Background()); len(targets) != 0 {
		t.Errorf("targets stored: %d, want none", len(targets))
	}
}

// attach-info says what the task was launched with, not the target's pin
// now.
func TestAttachInfo_AgentArgs(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	ctx := context.Background()
	target := &registry.Target{ID: "t1", Name: "sc1", Kind: registry.TargetKindRemote, Host: "sc1.example", User: "u",
		AgentArgs: map[string]registry.AgentArgs{"claude-code": {Model: "sonnet", Effort: "low"}}}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	ws := &registry.Workspace{ID: "w1", Name: "w1", Path: "/w", TargetID: "t1", Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for id, args := range map[string]registry.AgentArgs{"pinned": {Model: "opus", Effort: "high"}, "plain": {}} {
		if err := store.CreateTask(ctx, &registry.Task{ID: id, WorkspaceID: "w1", Kind: registry.TaskKindAgent, AgentType: "claude-code",
			TmuxSession: "loomux-" + id, Status: registry.TaskStatusAwaitingInput, AgentArgs: args}); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{
		"pinned": `"agent_args":{"model":"opus","effort":"high"}`,
		"plain":  `"agent_args":{"model":"","effort":""}`,
	} {
		status, body := doJSON(t, http.MethodGet, srv.URL+"/api/v1/tasks/"+id+"/attach-info", token, nil)
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("attach-info %s = %d %s, want %s", id, status, body, want)
		}
	}
}
