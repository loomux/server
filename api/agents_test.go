package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

type targetAgentJSON struct {
	AgentType string    `json:"agent_type"`
	Available bool      `json:"available"`
	Path      string    `json:"path"`
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
}

type fakeProber struct {
	calls  []string
	result []*registry.TargetAgent
	err    error
}

func (p *fakeProber) RefreshTargetAgents(ctx context.Context, targetID string) ([]*registry.TargetAgent, error) {
	p.calls = append(p.calls, targetID)
	return p.result, p.err
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

// POST /targets/{id}/agents/refresh re-probes through the configured
// prober and returns the fresh results.
func TestRefreshTargetAgents(t *testing.T) {
	prober := &fakeProber{result: []*registry.TargetAgent{
		{TargetID: "t1", AgentType: "claude-code", Available: true, Path: "/home/u/.local/bin/claude",
			Version: "2.1.287 (Claude Code)", CheckedAt: time.Now()},
	}}
	srv, _, _ := newTestServer(t, api.WithAgentProber(prober))
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/agents/refresh", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(prober.calls) != 1 || prober.calls[0] != "t1" {
		t.Errorf("prober calls = %v, want one for t1", prober.calls)
	}
	if got := decodeAgents(t, resp); len(got) != 1 || got[0].AgentType != "claude-code" || !got[0].Available ||
		got[0].Path != "/home/u/.local/bin/claude" || got[0].Version != "2.1.287 (Claude Code)" {
		t.Errorf("agents = %+v", got)
	}
}

func TestRefreshTargetAgents_Errors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("router: %w", registry.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("probe: %w", targets.ErrUnreachable), http.StatusBadGateway},
		{errors.New("boom"), http.StatusInternalServerError},
	} {
		srv, _, _ := newTestServer(t, api.WithAgentProber(&fakeProber{err: tc.err}))
		token, _ := login(t, srv.URL, testPassword)
		resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/agents/refresh", token, nil)
		if resp.StatusCode != tc.want {
			t.Errorf("prober error %v: status = %d, want %d", tc.err, resp.StatusCode, tc.want)
		}
		assertErrorEnvelope(t, resp)
		resp.Body.Close()
	}

	// Without a prober configured the endpoint says so instead of 404ing
	// like an unknown route.
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/agents/refresh", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("no prober: status = %d, want 501", resp.StatusCode)
	}
}
