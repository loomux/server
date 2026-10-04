package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

type fakeWorkspaceManager struct {
	deleted   []string
	left      []string
	statusSet []string
	err       error
}

func (m *fakeWorkspaceManager) DeleteWorkspace(ctx context.Context, id string) ([]string, error) {
	m.deleted = append(m.deleted, id)
	return m.left, m.err
}

func (m *fakeWorkspaceManager) SetWorkspaceStatus(ctx context.Context, id string, status registry.WorkspaceStatus) error {
	m.statusSet = append(m.statusSet, id+"="+string(status))
	return m.err
}

func TestDeleteWorkspace(t *testing.T) {
	m := &fakeWorkspaceManager{left: []string{"loomux-abc"}}
	srv, _, _ := newTestServer(t, api.WithWorkspaceManager(m))
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/workspaces/ws-1", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		SessionsNotKilled []string `json:"sessions_not_killed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.deleted) != 1 || m.deleted[0] != "ws-1" {
		t.Fatalf("deleted = %v, want [ws-1]", m.deleted)
	}
	if len(out.SessionsNotKilled) != 1 || out.SessionsNotKilled[0] != "loomux-abc" {
		t.Fatalf("sessions_not_killed = %v", out.SessionsNotKilled)
	}
}

func TestDeleteWorkspace_Errors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"unknown", registry.ErrNotFound, http.StatusNotFound, "no such workspace"},
		{"busy", &registry.ConflictError{Reason: "a task in this workspace is mid-turn"}, http.StatusConflict, "a task in this workspace is mid-turn"},
		{"other", errors.New("boom"), http.StatusInternalServerError, "could not delete workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t, api.WithWorkspaceManager(&fakeWorkspaceManager{err: tc.err}))
			token, _ := login(t, srv.URL, testPassword)
			resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/workspaces/ws-1", token, nil)
			defer resp.Body.Close()
			var out struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != tc.status || out.Error != tc.message {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, out.Error, tc.status, tc.message)
			}
		})
	}
}

func TestPatchWorkspaceStatus(t *testing.T) {
	m := &fakeWorkspaceManager{}
	srv, _, _ := newTestServer(t, api.WithWorkspaceManager(m))
	token, _ := login(t, srv.URL, testPassword)

	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"status":"idle"}`, http.StatusNoContent},
		{`{"status":"archived"}`, http.StatusNoContent},
		{`{"status":"active"}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	} {
		resp := authedRequest(t, http.MethodPatch, srv.URL+"/api/v1/workspaces/ws-1", token, []byte(tc.body))
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("PATCH %s: status = %d, want %d", tc.body, resp.StatusCode, tc.status)
		}
	}
	if len(m.statusSet) != 2 || m.statusSet[0] != "ws-1=idle" || m.statusSet[1] != "ws-1=archived" {
		t.Fatalf("status set = %v", m.statusSet)
	}

	m.err = &registry.ConflictError{Reason: "its directory /x doesn't exist on the target"}
	resp := authedRequest(t, http.MethodPatch, srv.URL+"/api/v1/workspaces/ws-1", token, []byte(`{"status":"idle"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict: status = %d, want 409", resp.StatusCode)
	}
}

func TestWorkspaceManage_NotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/workspaces/ws-1", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestWorkspaceManage_RequiresAuth(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithWorkspaceManager(&fakeWorkspaceManager{}))
	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/workspaces/ws-1", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
