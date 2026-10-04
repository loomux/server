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
)

type targetHealthJSON struct {
	Status        string    `json:"status"`
	Reachable     bool      `json:"reachable"`
	Error         string    `json:"error"`
	LatencyMS     int64     `json:"latency_ms"`
	TmuxVersion   string    `json:"tmux_version"`
	DiskFreeBytes *int64    `json:"disk_free_bytes"`
	LastProbedAt  time.Time `json:"last_probed_at"`
}

type fakeTargetProber struct {
	calls  []string
	health *registry.TargetHealth
	agents []*registry.TargetAgent
	err    error
}

func (p *fakeTargetProber) ProbeTarget(ctx context.Context, targetID string) (*registry.TargetHealth, []*registry.TargetAgent, error) {
	p.calls = append(p.calls, targetID)
	return p.health, p.agents, p.err
}

// GET /targets shows each target's last health probe (LOOM-86): null when
// never probed.
func TestListTargets_IncludesHealth(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	ctx := context.Background()
	for _, tg := range []*registry.Target{
		{ID: "t1", Name: "jet01", Kind: registry.TargetKindLocal},
		{ID: "t2", Name: "jet02", Kind: registry.TargetKindLocal},
		{ID: "t3", Name: "jet03", Kind: registry.TargetKindLocal},
	} {
		if err := store.CreateTarget(ctx, tg); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}
	probed := time.Now().UTC().Truncate(time.Second)
	for _, h := range []*registry.TargetHealth{
		{TargetID: "t1", Reachable: true, Latency: 120 * time.Millisecond, TmuxVersion: "tmux 3.4", DiskFreeBytes: 5 << 30, ProbedAt: probed},
		{TargetID: "t2", Reachable: false, Error: "unreachable: no answer", DiskFreeBytes: -1, ProbedAt: probed},
	} {
		if err := store.SetTargetHealth(ctx, h); err != nil {
			t.Fatalf("SetTargetHealth: %v", err)
		}
	}

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/targets", token, nil)
	defer resp.Body.Close()
	var out struct {
		Targets []struct {
			ID     string            `json:"id"`
			Health *targetHealthJSON `json:"health"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]*targetHealthJSON{}
	for _, tg := range out.Targets {
		byID[tg.ID] = tg.Health
	}
	if h := byID["t1"]; h == nil || h.Status != "healthy" || !h.Reachable || h.LatencyMS != 120 ||
		h.TmuxVersion != "tmux 3.4" || h.DiskFreeBytes == nil || *h.DiskFreeBytes != 5<<30 || !h.LastProbedAt.Equal(probed) {
		t.Errorf("t1 health = %+v", h)
	}
	if h := byID["t2"]; h == nil || h.Status != "unhealthy" || h.Reachable || h.Error != "unreachable: no answer" || h.DiskFreeBytes != nil {
		t.Errorf("t2 health = %+v", h)
	}
	if h, ok := byID["t3"]; !ok || h != nil {
		t.Errorf("t3 health = %+v (listed %v), want null: never probed", h, ok)
	}
}

// POST /targets/{id}/probe probes now and returns health and agents.
func TestProbeTarget(t *testing.T) {
	prober := &fakeTargetProber{
		health: &registry.TargetHealth{TargetID: "t1", Reachable: true, TmuxVersion: "tmux 3.4", DiskFreeBytes: 1 << 30, ProbedAt: time.Now()},
		agents: []*registry.TargetAgent{{TargetID: "t1", AgentType: "claude-code", Available: true,
			AuthStatus: registry.AgentAuthLoggedOut, CheckedAt: time.Now()}},
	}
	srv, _, _ := newTestServer(t, api.WithTargetProber(prober))
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/probe", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Health *targetHealthJSON `json:"health"`
		Agents []struct {
			AgentType  string `json:"agent_type"`
			AuthStatus string `json:"auth_status"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(prober.calls) != 1 || prober.calls[0] != "t1" {
		t.Errorf("prober calls = %v", prober.calls)
	}
	if out.Health == nil || out.Health.Status != "healthy" || out.Health.TmuxVersion != "tmux 3.4" {
		t.Errorf("health = %+v", out.Health)
	}
	if len(out.Agents) != 1 || out.Agents[0].AuthStatus != registry.AgentAuthLoggedOut {
		t.Errorf("agents = %+v, want claude-code logged out", out.Agents)
	}
}

func TestProbeTarget_Errors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("router: %w", registry.ErrNotFound), http.StatusNotFound},
		{errors.New("boom"), http.StatusInternalServerError},
	} {
		srv, _, _ := newTestServer(t, api.WithTargetProber(&fakeTargetProber{err: tc.err}))
		token, _ := login(t, srv.URL, testPassword)
		resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/probe", token, nil)
		if resp.StatusCode != tc.want {
			t.Errorf("prober error %v: status = %d, want %d", tc.err, resp.StatusCode, tc.want)
		}
		assertErrorEnvelope(t, resp)
		resp.Body.Close()
	}

	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/t1/probe", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("no prober: status = %d, want 501", resp.StatusCode)
	}
}
