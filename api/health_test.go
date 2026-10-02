package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/internal/health"
)

type fakeHealthChecker struct {
	shallow health.Result
	deep    health.Result
}

func (f *fakeHealthChecker) Shallow(ctx context.Context) health.Result { return f.shallow }
func (f *fakeHealthChecker) Deep(ctx context.Context) health.Result    { return f.deep }

func TestHealth_Unauthenticated_ReturnsShallow(t *testing.T) {
	checker := &fakeHealthChecker{
		shallow: health.Result{
			Status: health.StatusHealthy,
			Components: map[string]health.ComponentResult{
				"database":     {Status: health.StatusHealthy},
				"router_model": {Status: health.StatusHealthy},
			},
		},
	}
	srv, _, _ := newTestServer(t, api.WithHealthChecker(checker))

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var result health.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != health.StatusHealthy {
		t.Errorf("status = %q, want healthy", result.Status)
	}
}

func TestHealth_DegradedReturns503(t *testing.T) {
	checker := &fakeHealthChecker{
		shallow: health.Result{
			Status: health.StatusDegraded,
			Components: map[string]health.ComponentResult{
				"database": {Status: health.StatusUnhealthy, Error: "db closed"},
			},
		},
	}
	srv, _, _ := newTestServer(t, api.WithHealthChecker(checker))

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHealthDeep_RequiresAuth(t *testing.T) {
	checker := &fakeHealthChecker{
		deep: health.Result{
			Status: health.StatusHealthy,
			Components: map[string]health.ComponentResult{
				"targets": {Status: health.StatusHealthy},
			},
		},
	}
	srv, _, _ := newTestServer(t, api.WithHealthChecker(checker))

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/health/deep", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHealthDeep_AuthenticatedReturnsDeep(t *testing.T) {
	checker := &fakeHealthChecker{
		deep: health.Result{
			Status: health.StatusHealthy,
			Components: map[string]health.ComponentResult{
				"targets": {Status: health.StatusHealthy},
			},
		},
	}
	srv, _, _ := newTestServer(t, api.WithHealthChecker(checker))
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/health/deep", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var result health.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := result.Components["targets"]; !ok {
		t.Error("expected targets component in deep health response")
	}
}

func TestHealth_NotConfiguredReturns503(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// Compile-time check that fakeHealthChecker satisfies api.HealthChecker.
var _ interface {
	Shallow(ctx context.Context) health.Result
	Deep(ctx context.Context) health.Result
} = (*fakeHealthChecker)(nil)
