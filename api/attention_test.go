package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-97: a needs-attention task's prompt reaches the client through
// the conversation detail, for the web's needs-attention card.
func TestAttentionExposed(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws-a", registry.WorkspaceStatusActive)
	task := &registry.Task{
		ID: "task-a", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code", TmuxSession: "s",
		ConversationID: "conv-a", Status: registry.TaskStatusNeedsAttention,
		Attention: &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command", Detail: "rm -rf build",
			Question: "Do you want to proceed?", Options: []registry.AttentionOption{{Label: "Yes"}, {Label: "No"}}},
	}
	if err := store.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-a", token, nil)
	defer resp.Body.Close()
	var conv struct {
		Tasks []struct {
			Status    string              `json:"status"`
			Attention *registry.Attention `json:"attention"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(conv.Tasks) != 1 || conv.Tasks[0].Status != "needs_attention" || conv.Tasks[0].Attention == nil ||
		conv.Tasks[0].Attention.Detail != "rm -rf build" || len(conv.Tasks[0].Attention.Options) != 2 {
		t.Errorf("tasks = %+v", conv.Tasks)
	}
}
