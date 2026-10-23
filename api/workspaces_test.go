package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-215: POST /api/v1/workspaces and GET /api/v1/workspaces/{id}.

// stubProvisioner answers as set and records what it was asked.
type stubProvisioner struct {
	got  api.WorkspaceToProvision
	resp api.ProvisionedWorkspace
	err  error
}

func (s *stubProvisioner) ProvisionWorkspace(ctx context.Context, req api.WorkspaceToProvision) (api.ProvisionedWorkspace, error) {
	s.got = req
	return s.resp, s.err
}

func provisioningServer(t *testing.T, p api.WorkspaceProvisioner) (string, string, registry.Store) {
	t.Helper()
	var opts []api.Option
	if p != nil {
		opts = append(opts, api.WithWorkspaceProvisioner(p))
	}
	srv, _, store := newTestServer(t, opts...)
	token, _ := login(t, srv.URL, testPassword)
	return srv.URL, token, store
}

type createdWorkspace struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	TargetID       string `json:"target_id"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason"`
	ConversationID string `json:"conversation_id"`
}

func decodeWorkspace(t *testing.T, resp *http.Response) createdWorkspace {
	t.Helper()
	var ws createdWorkspace
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCreateWorkspace_Accepted(t *testing.T) {
	stub := &stubProvisioner{resp: api.ProvisionedWorkspace{
		Workspace:      &registry.Workspace{ID: "ws-1", Name: "api-ws", TargetID: "t-1", Status: registry.WorkspaceStatusProvisioning},
		ConversationID: "conv-provision",
	}}
	base, token, _ := provisioningServer(t, stub)
	resp := authedRequest(t, http.MethodPost, base+"/api/v1/workspaces", token,
		[]byte(`{"target_id": "t-1", "name": "api-ws", "git_remote": "https://github.com/acme/x.git", "tags": ["go"]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Location") != "/api/v1/workspaces/ws-1" {
		t.Fatalf("status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	ws := decodeWorkspace(t, resp)
	if ws.ID != "ws-1" || ws.Status != "provisioning" || ws.ConversationID != "conv-provision" {
		t.Errorf("body = %+v", ws)
	}
	if g := stub.got; g.TargetID != "t-1" || g.Name != "api-ws" || g.Kind != "git_clone" || g.GitRemote != "https://github.com/acme/x.git" || len(g.Tags) != 1 {
		t.Errorf("asked = %+v (a git_remote with no kind is a clone)", g)
	}
	resp = authedRequest(t, http.MethodPost, base+"/api/v1/workspaces", token, []byte(`{"target_id": "t-1", "name": "plain"}`))
	resp.Body.Close()
	if stub.got.Kind != "empty" {
		t.Errorf("no kind, no remote: %q, want empty", stub.got.Kind)
	}
}

func TestCreateWorkspace_Existing(t *testing.T) {
	stub := &stubProvisioner{resp: api.ProvisionedWorkspace{
		Workspace: &registry.Workspace{ID: "ws-1", Name: "api-ws", TargetID: "t-1", Status: registry.WorkspaceStatusIdle},
		Existing:  true,
	}}
	base, token, _ := provisioningServer(t, stub)
	resp := authedRequest(t, http.MethodPost, base+"/api/v1/workspaces", token, []byte(`{"target_id": "t-1", "name": "api-ws"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a repeat: status %d, want 200", resp.StatusCode)
	}
	if ws := decodeWorkspace(t, resp); ws.ID != "ws-1" || ws.Status != "idle" || ws.ConversationID != "" {
		t.Errorf("body = %+v", ws)
	}
}

func TestCreateWorkspace_Errors(t *testing.T) {
	cases := []struct {
		name, body string
		err        error
		status     int
		code, text string
	}{
		{"no target", `{"name": "x"}`, nil, http.StatusBadRequest, "invalid_request", "target_id is required"},
		{"no name", `{"target_id": "t-1"}`, nil, http.StatusBadRequest, "invalid_request", "name is required"},
		{"not JSON", `{`, nil, http.StatusBadRequest, "invalid_request", ""},
		{"unknown target", `{"target_id": "t-9", "name": "x"}`, api.ErrNoSuchTarget, http.StatusBadRequest, "invalid_request", "no such target"},
		{"bad spec", `{"target_id": "t-1", "name": "Bad Name"}`, &api.WorkspaceSpecError{Msg: `workspace name "Bad Name" must be lowercase`}, http.StatusBadRequest, "invalid_request", "must be lowercase"},
		{"name taken", `{"target_id": "t-1", "name": "x"}`, &api.WorkspaceNameTakenError{Msg: `workspace "x" exists on another target`}, http.StatusConflict, "workspace_name_taken", "exists on another target"},
		{"store", `{"target_id": "t-1", "name": "x"}`, errors.New("disk full"), http.StatusInternalServerError, "internal", "could not provision"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, token, _ := provisioningServer(t, &stubProvisioner{err: c.err})
			resp := authedRequest(t, http.MethodPost, base+"/api/v1/workspaces", token, []byte(c.body))
			defer resp.Body.Close()
			var body struct{ Error, Code string }
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if resp.StatusCode != c.status || body.Code != c.code || !strings.Contains(body.Error, c.text) {
				t.Errorf("status %d %+v, want %d %s %q", resp.StatusCode, body, c.status, c.code, c.text)
			}
			if strings.Contains(body.Error, "disk full") {
				t.Error("an internal error's text reached the client")
			}
		})
	}
}

func TestCreateWorkspace_AuthAndConfig(t *testing.T) {
	base, _, _ := provisioningServer(t, &stubProvisioner{})
	resp, err := http.Post(base+"/api/v1/workspaces", "application/json", strings.NewReader(`{"target_id": "t-1", "name": "x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", resp.StatusCode)
	}
	base, token, _ := provisioningServer(t, nil)
	resp = authedRequest(t, http.MethodPost, base+"/api/v1/workspaces", token, []byte(`{"target_id": "t-1", "name": "x"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("without a provisioner: %d, want 501", resp.StatusCode)
	}
}

func TestGetWorkspace(t *testing.T) {
	base, token, store := provisioningServer(t, nil)
	ws := createTestWorkspace(t, store, "ws-get", registry.WorkspaceStatusFailed)
	cur, _ := store.GetWorkspace(context.Background(), ws.ID)
	cur.StatusReason = "provisioning failed at run: exit 1"
	if err := store.UpdateWorkspace(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	resp := authedRequest(t, http.MethodGet, base+"/api/v1/workspaces/"+ws.ID, token, nil)
	defer resp.Body.Close()
	got := decodeWorkspace(t, resp)
	if resp.StatusCode != http.StatusOK || got.ID != ws.ID || got.Status != "failed" || got.StatusReason != "provisioning failed at run: exit 1" {
		t.Errorf("GET = %d %+v", resp.StatusCode, got)
	}
	resp = authedRequest(t, http.MethodGet, base+"/api/v1/workspaces/no-such-id", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", resp.StatusCode)
	}
}
