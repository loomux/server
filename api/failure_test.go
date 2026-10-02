package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-77: a failed task's reason, class, output tail and (for a command
// task) command + exit code reach the client through the conversation
// detail, and a workspace's status_reason through the workspace list —
// enough for the web to render a failure card instead of a bare "failed".
func TestFailureDetailsExposed(t *testing.T) {
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	ws := createTestWorkspace(t, store, "ws-f", registry.WorkspaceStatusFailed)
	ws.StatusReason = "provisioning failed at wait for completion: provisioning command exited with status 3"
	if err := store.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	code := 127
	task := &registry.Task{
		ID: "task-f", WorkspaceID: ws.ID, Kind: registry.TaskKindCommand, TmuxSession: "s", ConversationID: "conv-f",
		Status: registry.TaskStatusFailed, Command: "codex", ExitCode: &code,
		FailureReason: "agent exited", ErrorClass: registry.ErrorClassAgentExited, OutputTail: "codex: not found",
	}
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-f", token, nil)
	defer resp.Body.Close()
	var conv struct {
		Tasks []struct {
			FailureReason string `json:"failure_reason"`
			ErrorClass    string `json:"error_class"`
			OutputTail    string `json:"output_tail"`
			Command       string `json:"command"`
			ExitCode      *int   `json:"exit_code"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if len(conv.Tasks) != 1 {
		t.Fatalf("tasks = %+v", conv.Tasks)
	}
	got := conv.Tasks[0]
	if got.FailureReason != "agent exited" || got.ErrorClass != "agent_exited" || got.OutputTail != "codex: not found" ||
		got.Command != "codex" || got.ExitCode == nil || *got.ExitCode != 127 {
		t.Errorf("conversation task = %+v (exit %v), want the failure details", got, got.ExitCode)
	}

	wsResp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/workspaces", token, nil)
	defer wsResp.Body.Close()
	var list struct {
		Workspaces []struct {
			Status       string `json:"status"`
			StatusReason string `json:"status_reason"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(wsResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode workspaces: %v", err)
	}
	if len(list.Workspaces) != 1 || list.Workspaces[0].Status != "failed" || list.Workspaces[0].StatusReason != ws.StatusReason {
		t.Errorf("workspaces = %+v, want failed with its status_reason", list.Workspaces)
	}
}
