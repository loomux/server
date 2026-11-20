package storetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-307: a target's agent_args round-trip through create, update and
// list; an update that leaves them alone keeps them; an empty map reads
// back as none.
func testTargetAgentArgs(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	got, err := s.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if len(got.AgentArgs) != 0 {
		t.Fatalf("new target's agent_args = %+v, want none", got.AgentArgs)
	}

	want := map[string]registry.AgentArgs{
		"claude-code": {Model: "opus", Effort: "high"},
		"codex":       {Effort: "medium"},
	}
	pinned := &registry.Target{ID: "pinned", Name: "pinned", Kind: registry.TargetKindLocal, AgentArgs: want}
	if err := s.CreateTarget(ctx, pinned); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if got, err := s.GetTarget(ctx, "pinned"); err != nil || !reflect.DeepEqual(got.AgentArgs, want) {
		t.Fatalf("GetTarget(pinned).AgentArgs = %+v, %v; want %+v", got.AgentArgs, err, want)
	}

	got.AgentArgs = map[string]registry.AgentArgs{"codex": {Model: "gpt-5.1-codex"}}
	if err := s.UpdateTarget(ctx, got); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
	list, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	byID := map[string]*registry.Target{}
	for _, tt := range list {
		byID[tt.ID] = tt
	}
	if !reflect.DeepEqual(byID[target.ID].AgentArgs, got.AgentArgs) {
		t.Fatalf("updated agent_args = %+v, want %+v", byID[target.ID].AgentArgs, got.AgentArgs)
	}
	if !reflect.DeepEqual(byID["pinned"].AgentArgs, want) {
		t.Fatalf("other target's agent_args = %+v, want %+v (untouched)", byID["pinned"].AgentArgs, want)
	}

	got.AgentArgs = map[string]registry.AgentArgs{}
	if err := s.UpdateTarget(ctx, got); err != nil {
		t.Fatalf("UpdateTarget (clear): %v", err)
	}
	if cleared, err := s.GetTarget(ctx, target.ID); err != nil || len(cleared.AgentArgs) != 0 {
		t.Fatalf("cleared agent_args = %+v, %v; want none", cleared.AgentArgs, err)
	}
}

// LOOM-307: a task records the model and effort it was launched with;
// later updates keep them, whatever the struct passed holds.
func testTaskAgentArgs(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	want := registry.AgentArgs{Model: "opus", Effort: "high"}
	task := &registry.Task{ID: "pinned-task", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-pinned", Status: registry.TaskStatusRunning, ConversationID: "conv-1", AgentArgs: want}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil || got.AgentArgs != want {
		t.Fatalf("GetTask.AgentArgs = %+v, %v; want %+v", got.AgentArgs, err, want)
	}
	got.Status = registry.TaskStatusCompleted
	got.AgentArgs = registry.AgentArgs{}
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	tasks, err := s.ListTasksByWorkspace(ctx, ws.ID)
	if err != nil || len(tasks) != 1 || tasks[0].AgentArgs != want {
		t.Fatalf("ListTasks = %+v, %v; want one task with AgentArgs %+v", tasks, err, want)
	}

	plain := &registry.Task{ID: "plain-task", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "codex",
		TmuxSession: "loomux-plain", Status: registry.TaskStatusRunning, ConversationID: "conv-1"}
	if err := s.CreateTask(ctx, plain); err != nil {
		t.Fatalf("CreateTask(plain): %v", err)
	}
	if got, err := s.GetTask(ctx, plain.ID); err != nil || got.AgentArgs != (registry.AgentArgs{}) {
		t.Fatalf("unpinned task's AgentArgs = %+v, %v; want zero", got.AgentArgs, err)
	}
}
