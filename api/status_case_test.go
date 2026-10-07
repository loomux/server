package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// API v1 freeze review item 9: task statuses are snake_case in the API,
// like every other enum — in the conversation list, a conversation's
// tasks and the stream's task_update — whatever the store spells them.
func TestTaskStatusesAreSnakeCase(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	ws := createTestWorkspace(t, store, "ws-case", registry.WorkspaceStatusIdle)
	createTestTask(t, store, "task-case", ws.ID, "conv-case", registry.TaskStatusAwaitingInput)
	token, _ := login(t, srv.URL, testPassword)

	var list struct {
		Conversations []struct {
			Status string `json:"status"`
		} `json:"conversations"`
	}
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations", token, nil)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Conversations) != 1 || list.Conversations[0].Status != "awaiting_input" {
		t.Errorf("conversation list = %+v, want status awaiting_input", list)
	}

	var conv struct {
		Tasks []struct {
			Status string `json:"status"`
		} `json:"tasks"`
	}
	resp = authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-case", token, nil)
	_ = json.NewDecoder(resp.Body).Decode(&conv)
	resp.Body.Close()
	if len(conv.Tasks) != 1 || conv.Tasks[0].Status != "awaiting_input" {
		t.Errorf("conversation tasks = %+v, want status awaiting_input", conv)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/conversations/conv-case/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Body.Close()
	ev := readSSEEventWithTimeout(t, bufio.NewReader(stream.Body), 2*time.Second)
	var update struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &update); err != nil || ev.Event != "task_update" || update.Status != "awaiting_input" {
		t.Errorf("stream event %q %s, want task_update with status awaiting_input", ev.Event, ev.Data)
	}
}

// permission_mode is snake_case too (#269 review): accept_edits out; in,
// both accept_edits and the pre-1.0 accept-edits, stored as before.
func TestPermissionModeSnakeCase(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	for _, in := range []string{"accept_edits", "accept-edits"} {
		t.Run(in, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"name": "box-" + in, "kind": "remote", "host": "box", "user": "u",
				"permission_mode": in})
			resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", token, body)
			defer resp.Body.Close()
			var out struct {
				ID             string `json:"id"`
				PermissionMode string `json:"permission_mode"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != http.StatusCreated || out.PermissionMode != "accept_edits" {
				t.Fatalf("POST permission_mode %q = %d %+v, want 201 and accept_edits", in, resp.StatusCode, out)
			}
			stored, _ := store.GetTarget(context.Background(), out.ID)
			if stored.PermissionMode != registry.PermissionModeAcceptEdits {
				t.Errorf("stored = %q, want the store's spelling", stored.PermissionMode)
			}
		})
	}
}
