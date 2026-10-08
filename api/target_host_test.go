package api_test

import (
	"net/http"
	"testing"
)

// LOOM-175: a remote target's host must be a host name, an ssh_config
// alias or an IP address, on create and on update alike.
func TestTargets_HostGrammar(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	for _, host := range []string{"box.example.net", "work_laptop", "10.0.0.7", "fd7a:115c::1"} {
		if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "ok-" + host, "kind": "remote", "host": host, "user": "loomux"}); status != http.StatusCreated {
			t.Errorf("create with host %q: %d, want 201", host, status)
		}
	}
	bad := []string{"a..b", "box%h", "box;id", "[::1]", "-box", "box name", "box/x", "box-.net"}
	for _, host := range bad {
		if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "bad", "kind": "remote", "host": host, "user": "loomux"}); status != http.StatusBadRequest {
			t.Errorf("create with host %q: %d, want 400", host, status)
		}
	}
	ok, status := createTarget(t, srv.URL, token, map[string]any{"name": "r", "kind": "remote", "host": "h.example.net", "user": "loomux"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}
	for _, host := range bad {
		resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/"+ok.ID, token, mustJSON(t, map[string]any{"name": "r", "kind": "remote", "host": host, "user": "loomux"}))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("update to host %q: %d, want 400", host, resp.StatusCode)
		}
	}
}
