package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// GET /tasks/{id}/transcript (LOOM-91) returns what each turn of a task
// produced, oldest first — after the task has ended too.
func TestTaskTranscript(t *testing.T) {
	srv, _, _ := newTestServer(t) // no transcript store configured
	srv2, _, store2 := newTestServerWith(t, func(s registry.Store) []api.Option { return []api.Option{api.WithTaskTurns(s)} })
	token, _ := login(t, srv2.URL, testPassword)
	ctx := context.Background()
	ws := createTestWorkspace(t, store2, "transcript", registry.WorkspaceStatusIdle)
	task := &registry.Task{ID: "task-1", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-x", Status: registry.TaskStatusCompleted, ConversationID: "conv"}
	if err := store2.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, msg := range []string{"first", "second"} {
		if err := store2.CreateTaskTurn(ctx, &registry.TaskTurn{ID: "turn-" + msg, TaskID: task.ID, UserMessage: msg,
			AgentMessage: "answer to " + msg, Pane: "pane " + msg}); err != nil {
			t.Fatalf("CreateTaskTurn: %v", err)
		}
	}

	resp := authedRequest(t, http.MethodGet, srv2.URL+"/api/v1/tasks/task-1/transcript", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		TaskID string `json:"task_id"`
		Turns  []struct {
			UserMessage  string    `json:"user_message"`
			AgentMessage string    `json:"agent_message"`
			Pane         string    `json:"pane"`
			CreatedAt    time.Time `json:"created_at"`
		} `json:"turns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TaskID != "task-1" || len(out.Turns) != 2 || out.Turns[0].UserMessage != "first" ||
		out.Turns[1].AgentMessage != "answer to second" || out.Turns[1].Pane != "pane second" || out.Turns[0].CreatedAt.IsZero() {
		t.Errorf("transcript = %+v", out)
	}

	missing := authedRequest(t, http.MethodGet, srv2.URL+"/api/v1/tasks/nope/transcript", token, nil)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown task: status = %d, want 404", missing.StatusCode)
	}
	assertErrorEnvelope(t, missing)

	// Not configured: 501, not the mux's 404.
	token1, _ := login(t, srv.URL, testPassword)
	off := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/tasks/task-1/transcript", token1, nil)
	defer off.Body.Close()
	if off.StatusCode != http.StatusNotImplemented {
		t.Errorf("no transcript store: status = %d, want 501", off.StatusCode)
	}
}

// LOOM-122: the transcript comes a page at a time, latest page first;
// next_before fetches the one before it.
func TestTaskTranscript_Paged(t *testing.T) {
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option { return []api.Option{api.WithTaskTurns(s)} })
	token, _ := login(t, srv.URL, testPassword)
	ctx := context.Background()
	ws := createTestWorkspace(t, store, "paged", registry.WorkspaceStatusIdle)
	if err := store.CreateTask(ctx, &registry.Task{ID: "task-p", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent,
		TmuxSession: "loomux-p", Status: registry.TaskStatusCompleted, ConversationID: "conv"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, msg := range []string{"1", "2", "3"} {
		if err := store.CreateTaskTurn(ctx, &registry.TaskTurn{ID: "turn-" + msg, TaskID: "task-p", UserMessage: msg}); err != nil {
			t.Fatalf("CreateTaskTurn: %v", err)
		}
	}
	type page struct {
		Turns []struct {
			ID          string `json:"id"`
			UserMessage string `json:"user_message"`
		} `json:"turns"`
		HasMore    bool   `json:"has_more"`
		NextBefore string `json:"next_before"`
	}
	get := func(query string) (page, int) {
		resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/tasks/task-p/transcript"+query, token, nil)
		defer resp.Body.Close()
		var p page
		_ = json.NewDecoder(resp.Body).Decode(&p)
		return p, resp.StatusCode
	}

	p, status := get("?limit=2")
	if status != http.StatusOK || len(p.Turns) != 2 || p.Turns[0].UserMessage != "2" || p.Turns[1].ID != "turn-3" ||
		!p.HasMore || p.NextBefore != "turn-2" {
		t.Fatalf("first page = %d %+v", status, p)
	}
	p, _ = get("?limit=2&before=" + p.NextBefore)
	if len(p.Turns) != 1 || p.Turns[0].UserMessage != "1" || p.HasMore || p.NextBefore != "" {
		t.Fatalf("second page = %+v", p)
	}
	for _, bad := range []string{"?limit=0", "?limit=x", "?limit=101"} {
		if _, status := get(bad); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, status)
		}
	}
	if _, status := get("?before=turn-nope"); status != http.StatusBadRequest {
		t.Errorf("unknown before: status %d, want 400", status)
	}
}
