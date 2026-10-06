package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// credentialServer is a test server whose vault has a master key.
func credentialServer(t *testing.T, opts ...api.Option) (baseURL, token string, vault registry.Store) {
	t.Helper()
	vault, err := sqlite.Open(filepath.Join(t.TempDir(), "vault.db"), sqlite.WithMasterKey(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vault.Close() })
	srv, _, _ := newTestServer(t, append(opts, api.WithCredentials(vault))...)
	token, _ = login(t, srv.URL, testPassword)
	return srv.URL, token, vault
}

func doJSON(t *testing.T, method, url, token string, body any) (int, string) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	resp := authedRequest(t, method, url, token, b)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// LOOM-134: create, list, rotate and delete a credential; the value goes
// in and never comes back out.
func TestCredentials_CRUDNeverReturnsValue(t *testing.T) {
	base, token, vault := credentialServer(t)
	ctx := context.Background()

	status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "GITHUB_TOKEN", "value": "ghp-SECRET-1"})
	if status != http.StatusCreated || strings.Contains(body, "SECRET") {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID, Name string }
	_ = json.Unmarshal([]byte(body), &created)

	status, body = doJSON(t, "GET", base+"/api/v1/credentials", token, nil)
	if status != http.StatusOK || !strings.Contains(body, `"name":"GITHUB_TOKEN"`) || strings.Contains(body, "SECRET") || strings.Contains(body, "value") {
		t.Fatalf("list: %d %s", status, body)
	}

	if status, body = doJSON(t, "PUT", base+"/api/v1/credentials/"+created.ID+"/value", token, map[string]string{"value": "ghp-SECRET-2"}); status != http.StatusNoContent {
		t.Fatalf("set value: %d %s", status, body)
	}
	if got, err := vault.GetCredential(ctx, created.ID); err != nil || got.Value != "ghp-SECRET-2" {
		t.Fatalf("stored after rotate: %+v, %v", got, err)
	}

	if status, body = doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "GITHUB_TOKEN", "value": "again"}); status != http.StatusConflict {
		t.Errorf("duplicate at the same scope: %d %s, want 409", status, body)
	}

	if status, body = doJSON(t, "DELETE", base+"/api/v1/credentials/"+created.ID, token, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	if status, _ = doJSON(t, "DELETE", base+"/api/v1/credentials/"+created.ID, token, nil); status != http.StatusNotFound {
		t.Errorf("delete again: %d, want 404", status)
	}
	if status, _ = doJSON(t, "PUT", base+"/api/v1/credentials/"+created.ID+"/value", token, map[string]string{"value": "x"}); status != http.StatusNotFound {
		t.Errorf("set value of a deleted one: %d, want 404", status)
	}
}

func TestCredentials_Validation(t *testing.T) {
	base, token, vault := credentialServer(t, api.WithAgentTypes([]string{"claude-code"}))
	for _, tc := range []struct {
		name string
		req  map[string]string
	}{
		{"bad name", map[string]string{"name": "1TOKEN", "value": "v"}},
		{"name with a dash", map[string]string{"name": "MY-TOKEN", "value": "v"}},
		{"reserved name", map[string]string{"name": "PATH", "value": "v"}},
		{"Loomux's own", map[string]string{"name": "LOOMUX_MARKER_PATH", "value": "v"}},
		{"empty value", map[string]string{"name": "TOKEN", "value": ""}},
		{"NUL in value", map[string]string{"name": "TOKEN", "value": "a\x00b"}},
		{"unknown agent type", map[string]string{"name": "TOKEN", "value": "v", "agent_type": "nope"}},
		{"no such workspace", map[string]string{"name": "TOKEN", "value": "v", "workspace_id": "ws-missing"}},
	} {
		if status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, tc.req); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tc.name, status, body)
		}
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "TOKEN", "value": strings.Repeat("x", 64<<10+1)}); status != http.StatusBadRequest {
		t.Errorf("too long: %d %s", status, body)
	}

	// Scoped to a workspace and an agent type.
	ws := createTestWorkspace(t, vault, "scoped", registry.WorkspaceStatusIdle)
	if status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "TOKEN", "value": "v", "workspace_id": ws.ID, "agent_type": "claude-code"}); status != http.StatusCreated ||
		!strings.Contains(body, ws.ID) || !strings.Contains(body, "claude-code") {
		t.Errorf("scoped create: %d %s", status, body)
	}
}

func TestCredentials_RequireAuth(t *testing.T) {
	base, _, _ := credentialServer(t)
	for _, m := range []struct{ method, path string }{
		{"GET", "/api/v1/credentials"}, {"POST", "/api/v1/credentials"},
		{"PUT", "/api/v1/credentials/x/value"}, {"DELETE", "/api/v1/credentials/x"},
	} {
		if status, _ := doJSON(t, m.method, base+m.path, "", map[string]string{"name": "T", "value": "v"}); status != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", m.method, m.path, status)
		}
	}
}
