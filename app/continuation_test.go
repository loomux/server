package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
)

// TestBuild_EndToEnd_Continuation_RealTmux proves LOOM-13's
// task-continuation behavior through the full real composition — real
// sqlite, a real local tmux session (not a fake executor), real tiered
// completion detection — with only the LLM vendor call faked out. This
// is the router-layer continuation tests' assertions re-proven against
// real infrastructure, per LOOM-14 giving this a real composition root
// to test end-to-end against.
//
// The fake LLM server distinguishes Decide vs Relay calls by which tool
// the forced tool_choice names, and gives a different condense_output
// answer on the first vs second Relay call (Done: false, then Done:
// true) — so a single real tmux session carries two turns of an
// ongoing conversation before being torn down.
func TestBuild_EndToEnd_Continuation_RealTmux(t *testing.T) {
	ctx := context.Background()
	targetID := uuid.NewString()
	workspaceID := uuid.NewString()

	var relayCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolChoice struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_choice"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		var toolName, argsJSON string
		switch req.ToolChoice.Function.Name {
		case "route_decision":
			toolName = "route_decision"
			argsJSON = `{"action":"use_workspace","workspace_id":"` + workspaceID + `","agent_type":"echo-agent"}`
		case "condense_output":
			toolName = "condense_output"
			if atomic.AddInt32(&relayCalls, 1) == 1 {
				argsJSON = `{"reply":"turn one done, waiting for more","done":false}`
			} else {
				argsJSON = `{"reply":"all done now","done":true}`
			}
		}

		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": toolName, "arguments": argsJSON},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	seedStore, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open (seed): %v", err)
	}
	if err := seedStore.CreateTarget(ctx, &registry.Target{ID: targetID, Name: "t", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := seedStore.CreateWorkspace(ctx, &registry.Workspace{
		ID: workspaceID, Name: "ws", Path: t.TempDir(), TargetID: targetID, Status: registry.WorkspaceStatusIdle,
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("Close (seed): %v", err)
	}

	cfg := Config{
		DBPath:    dbPath,
		MasterKey: []byte("01234567890123456789012345678901"[:32]),
		Router: llmrouter.Config{
			Primary: llmrouter.Tier{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"},
		},
	}
	agentTypes := router.AgentTypeRegistry{
		"echo-agent": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 300 * time.Millisecond},
			LaunchTemplate: `sh -c 'echo continuation-test-output; sleep 30'`,
		},
	}

	a, err := build(cfg, agentTypes)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer a.Close()

	// Turn 1: launches a real tmux session; Relay says Done: false.
	reply, err := a.Dispatch(ctx, "conv-1", "start the thing", "")
	if err != nil {
		t.Fatalf("Dispatch (turn 1): %v", err)
	}
	if reply != "turn one done, waiting for more" {
		t.Fatalf("reply (turn 1) = %q, want %q", reply, "turn one done, waiting for more")
	}

	tasks, err := a.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task after turn 1", tasks)
	}
	firstTaskID := tasks[0].ID
	if tasks[0].Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task Status = %q, want %q", tasks[0].Status, registry.TaskStatusAwaitingInput)
	}

	realExec := targets.NewLocalExecutor()
	t.Cleanup(func() {
		if err := realExec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	exists, err := realExec.HasSession(ctx, tasks[0].TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (turn 1): %v", err)
	}
	if !exists {
		t.Fatal("real tmux session gone after turn 1, want it left alive (Done: false)")
	}

	ws, err := a.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace (turn 1): %v", err)
	}
	if ws.Status != registry.WorkspaceStatusActive {
		t.Fatalf("workspace Status (turn 1) = %q, want %q", ws.Status, registry.WorkspaceStatusActive)
	}

	// Turn 2: same conversation. Must reuse the same real tmux session
	// (SendMessage, not a fresh Launch) and, this time, Relay says
	// Done: true, so the session is torn down.
	reply, err = a.Dispatch(ctx, "conv-1", "keep going", "")
	if err != nil {
		t.Fatalf("Dispatch (turn 2): %v", err)
	}
	if reply != "all done now" {
		t.Fatalf("reply (turn 2) = %q, want %q", reply, "all done now")
	}

	tasks, err = a.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace (turn 2): %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want still exactly 1 task (reused, not a second one)", tasks)
	}
	if tasks[0].ID != firstTaskID {
		t.Fatalf("task ID = %q, want the same task reused from turn 1 (%q)", tasks[0].ID, firstTaskID)
	}
	if tasks[0].Status != registry.TaskStatusCompleted {
		t.Fatalf("task Status (turn 2) = %q, want %q", tasks[0].Status, registry.TaskStatusCompleted)
	}

	exists, err = realExec.HasSession(ctx, tasks[0].TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (turn 2): %v", err)
	}
	if exists {
		t.Fatal("real tmux session still alive after turn 2, want torn down (Done: true)")
	}

	ws, err = a.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GetWorkspace (turn 2): %v", err)
	}
	if ws.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status (turn 2) = %q, want %q", ws.Status, registry.WorkspaceStatusIdle)
	}
	if ws.RollingSummary != "all done now" {
		t.Fatalf("RollingSummary = %q, want %q", ws.RollingSummary, "all done now")
	}
}
