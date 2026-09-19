package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// TestBuild_IdleReaper_TearsDownRealSessionAutomatically proves LOOM-16
// end-to-end through the real composition: app.Build (via build, with a
// custom fast agent type) starts a real background reaper goroutine, and
// — with no further Dispatch calls at all — it discovers a real, idle
// tmux session on its own and tears it down, exactly the "verify this is
// actually exercised, don't assume it already works" the ticket asked
// for. A follow-up Dispatch for the same conversation afterward proves
// the router-level stale-session fallback (also LOOM-16) picks it back
// up with a fresh session, cooperating with the real reaper rather than
// just the simulated-kill router test.
func TestBuild_IdleReaper_TearsDownRealSessionAutomatically(t *testing.T) {
	ctx := context.Background()
	targetID := uuid.NewString()
	workspaceID := uuid.NewString()

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
			argsJSON = `{"reply":"still working on it","done":false}`
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
		ReapIdleThreshold: 100 * time.Millisecond,
		ReapInterval:      30 * time.Millisecond,
	}
	agentTypes := router.AgentTypeRegistry{
		"echo-agent": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 100 * time.Millisecond},
			LaunchTemplate: `sh -c 'echo reap-test-output; sleep 30'`,
		},
	}

	a, err := build(cfg, agentTypes)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer a.Close()

	reply, err := a.Dispatch(ctx, "conv-1", "start the thing", "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "still working on it" {
		t.Fatalf("reply = %q, want %q", reply, "still working on it")
	}

	tasks, err := a.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task", tasks)
	}
	task := tasks[0]

	realExec := targets.NewLocalExecutor()
	t.Cleanup(func() {
		if err := realExec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	exists, err := realExec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (before reap): %v", err)
	}
	if !exists {
		t.Fatal("real tmux session already gone right after Dispatch — test setup is wrong")
	}

	// No further Dispatch calls from here — only the real background
	// reaper goroutine (started inside build/Build) should tear this
	// down, entirely on its own.
	deadline := time.After(5 * time.Second)
	for {
		exists, err := realExec.HasSession(ctx, task.TmuxSession)
		if err != nil {
			t.Fatalf("HasSession (polling): %v", err)
		}
		if !exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("real reaper never tore down the idle session within the deadline")
		case <-time.After(30 * time.Millisecond):
		}
	}

	reaped, err := a.store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask (after reap): %v", err)
	}
	if reaped.ReapedAt == nil {
		t.Fatal("ReapedAt is nil after the session was torn down")
	}
	if reaped.Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task Status after reap = %q, want unchanged %q", reaped.Status, registry.TaskStatusAwaitingInput)
	}

	// Follow-up for the same conversation: the router-level stale-session
	// fallback (also LOOM-16) should transparently launch a fresh real
	// session rather than erroring out.
	reply, err = a.Dispatch(ctx, "conv-1", "keep going", "")
	if err != nil {
		t.Fatalf("Dispatch (after reap): %v", err)
	}
	if reply != "still working on it" {
		t.Fatalf("reply (after reap) = %q, want %q", reply, "still working on it")
	}

	tasksAfter, err := a.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace (after fallback): %v", err)
	}
	if len(tasksAfter) != 2 {
		t.Fatalf("ListTasksByWorkspace = %+v, want 2 tasks (the reaped one, now Failed, plus a fresh one)", tasksAfter)
	}
}
