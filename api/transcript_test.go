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
