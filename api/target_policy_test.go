package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
)

type targetPolicyJSON struct {
	ID                  string   `json:"id"`
	Purpose             string   `json:"purpose"`
	AllowedAgentTypes   []string `json:"allowed_agent_types"`
	AllowProvision      bool     `json:"allow_provision"`
	AllowShell          bool     `json:"allow_shell"`
	RequireConfirmation bool     `json:"require_confirmation"`
}

func targetRequestJSON(t *testing.T, method, url, token string, body map[string]any) (targetPolicyJSON, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp := authedRequest(t, method, url, token, raw)
	defer resp.Body.Close()
	var out targetPolicyJSON
	if resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out, resp.StatusCode
}

// LOOM-89: a target's policy is set on create, defaults to allowing
// everything, survives a PUT that doesn't mention it, and is validated.
func TestTargetPolicy_API(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	base := map[string]any{"name": "sc1", "kind": "remote", "host": "sc1", "user": "u"}

	def, status := targetRequestJSON(t, http.MethodPost, srv.URL+"/api/v1/targets", token, base)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	if def.Purpose != "" || !def.AllowProvision || !def.AllowShell || def.RequireConfirmation || len(def.AllowedAgentTypes) != 0 || def.AllowedAgentTypes == nil {
		t.Errorf("default policy = %+v, want everything allowed and allowed_agent_types []", def)
	}

	policy := map[string]any{"purpose": "work", "allowed_agent_types": []string{"claude-code"},
		"allow_provision": false, "allow_shell": false, "require_confirmation": true}
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	for k, v := range policy {
		body[k] = v
	}
	got, status := targetRequestJSON(t, http.MethodPut, srv.URL+"/api/v1/targets/"+def.ID, token, body)
	if status != http.StatusOK {
		t.Fatalf("update status = %d", status)
	}
	want := targetPolicyJSON{ID: def.ID, Purpose: "work", AllowedAgentTypes: []string{"claude-code"}, RequireConfirmation: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updated = %+v, want %+v", got, want)
	}

	// A PUT that doesn't mention the policy keeps it (LOOM-119).
	got, _ = targetRequestJSON(t, http.MethodPut, srv.URL+"/api/v1/targets/"+def.ID, token, base)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after a PUT without policy = %+v, want %+v", got, want)
	}

	bad := map[string]any{"purpose": "hobby"}
	for k, v := range base {
		bad[k] = v
	}
	if _, status := targetRequestJSON(t, http.MethodPut, srv.URL+"/api/v1/targets/"+def.ID, token, bad); status != http.StatusBadRequest {
		t.Errorf("bad purpose: status = %d, want 400", status)
	}
}

// LOOM-122: with the server's agent types known, an allowed_agent_types
// entry naming none of them is rejected, on create and on update.
func TestTargetPolicy_UnknownAgentType(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithAgentTypes([]string{"claude-code", "codex"}))
	token, _ := login(t, srv.URL, testPassword)
	base := map[string]any{"name": "jet01", "kind": "remote", "host": "jet01", "user": "u"}

	bad := map[string]any{"allowed_agent_types": []string{"claude-code", "claude"}}
	for k, v := range base {
		bad[k] = v
	}
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", token, mustJSON(t, bad))
	var out struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out.Error, `"claude"`) || !strings.Contains(out.Error, "claude-code, codex") {
		t.Fatalf("create with an unknown agent type = %d %q", resp.StatusCode, out.Error)
	}

	good := map[string]any{"allowed_agent_types": []string{"codex"}}
	for k, v := range base {
		good[k] = v
	}
	created, status := targetRequestJSON(t, http.MethodPost, srv.URL+"/api/v1/targets", token, good)
	if status != http.StatusCreated {
		t.Fatalf("create with a known agent type: status %d", status)
	}
	bad["name"] = "jet01"
	if _, status := targetRequestJSON(t, http.MethodPut, srv.URL+"/api/v1/targets/"+created.ID, token, bad); status != http.StatusBadRequest {
		t.Fatalf("update with an unknown agent type: status %d, want 400", status)
	}
}
