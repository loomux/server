package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

type targetAgentJSON struct {
	AgentType string    `json:"agent_type"`
	Available bool      `json:"available"`
	Path      string    `json:"path"`
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
}

func decodeAgents(t *testing.T, resp *http.Response) []targetAgentJSON {
	t.Helper()
	var out struct {
		Agents []targetAgentJSON `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode agents: %v", err)
	}
	return out.Agents
}

// GET /targets/{id}/agents (LOOM-71) reports the recorded agent CLI
// availability on a target — what the router last probed.
func TestListTargetAgents(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	target := &registry.Target{ID: "t1", Name: "jet01", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/targets/t1/agents", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := decodeAgents(t, resp); len(got) != 0 {
		t.Errorf("never-probed target agents = %+v, want empty list", got)
	}
	resp.Body.Close()

	if err := store.SetTargetAgent(context.Background(), &registry.TargetAgent{TargetID: "t1", AgentType: "codex", Available: false}); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
	resp = authedRequest(t, http.MethodGet, srv.URL+"/api/v1/targets/t1/agents", token, nil)
	defer resp.Body.Close()
	got := decodeAgents(t, resp)
	if len(got) != 1 || got[0].AgentType != "codex" || got[0].Available || got[0].CheckedAt.IsZero() {
		t.Errorf("agents = %+v, want codex recorded unavailable with a checked_at", got)
	}

	missing := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/targets/nope/agents", token, nil)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown target status = %d, want 404", missing.StatusCode)
	}
}
