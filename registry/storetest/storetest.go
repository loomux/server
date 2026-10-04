// Package storetest is the backend-conformance test suite for
// registry.Store. Any backend implementation should pass Run unmodified —
// that's what makes "adding a new backend means passing the existing
// suite" (design spec, Testing Strategy) literally true.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// Run executes the full conformance suite. newStore must return a fresh,
// empty Store for each call; the caller is responsible for its cleanup
// (e.g. via t.Cleanup).
func Run(t *testing.T, newStore func(t *testing.T) registry.Store) {
	t.Run("Target", func(t *testing.T) { testTargetCRUD(t, newStore(t)) })
	t.Run("TargetNotFound", func(t *testing.T) { testTargetNotFound(t, newStore(t)) })
	t.Run("TargetDuplicateName", func(t *testing.T) { testTargetDuplicateName(t, newStore(t)) })

	t.Run("TargetAgentUpsertAndList", func(t *testing.T) { testTargetAgentUpsertAndList(t, newStore(t)) })
	t.Run("TargetAgentPathAndVersion", func(t *testing.T) { testTargetAgentPathAndVersion(t, newStore(t)) })
	t.Run("TargetAgentRequiresValidTarget", func(t *testing.T) { testTargetAgentRequiresValidTarget(t, newStore(t)) })
	t.Run("TargetAgentDeletedWithTarget", func(t *testing.T) { testTargetAgentDeletedWithTarget(t, newStore(t)) })
	t.Run("TargetAgentAuthStatus", func(t *testing.T) { testTargetAgentAuthStatus(t, newStore(t)) })
	t.Run("TargetHealthRoundTrip", func(t *testing.T) { testTargetHealthRoundTrip(t, newStore(t)) })
	t.Run("TargetHealthRequiresValidTarget", func(t *testing.T) { testTargetHealthRequiresValidTarget(t, newStore(t)) })
	t.Run("TargetHealthDeletedWithTarget", func(t *testing.T) { testTargetHealthDeletedWithTarget(t, newStore(t)) })

	t.Run("TaskTurns", func(t *testing.T) { testTaskTurns(t, newStore(t)) })

	t.Run("Workspace", func(t *testing.T) { testWorkspaceCRUD(t, newStore(t)) })
	t.Run("WorkspaceStatusFailed", func(t *testing.T) { testWorkspaceStatusFailed(t, newStore(t)) })
	t.Run("WorkspaceNotFound", func(t *testing.T) { testWorkspaceNotFound(t, newStore(t)) })
	t.Run("WorkspaceDuplicateName", func(t *testing.T) { testWorkspaceDuplicateName(t, newStore(t)) })
	t.Run("WorkspaceRequiresValidTarget", func(t *testing.T) { testWorkspaceRequiresValidTarget(t, newStore(t)) })
	t.Run("WorkspaceRollingSummaryReplaces", func(t *testing.T) { testWorkspaceRollingSummaryReplaces(t, newStore(t)) })
	t.Run("WorkspaceTagsAndCapabilitiesRoundTrip", func(t *testing.T) { testWorkspaceTagsAndCapabilitiesRoundTrip(t, newStore(t)) })

	t.Run("Task", func(t *testing.T) { testTaskCRUD(t, newStore(t)) })
	t.Run("CommandTaskRoundTrip", func(t *testing.T) { testCommandTaskRoundTrip(t, newStore(t)) })
	t.Run("TaskAttentionRoundTrip", func(t *testing.T) { testTaskAttentionRoundTrip(t, newStore(t)) })
	t.Run("TaskFailureRoundTrip", func(t *testing.T) { testTaskFailureRoundTrip(t, newStore(t)) })
	t.Run("WorkspaceStatusReasonRoundTrip", func(t *testing.T) { testWorkspaceStatusReasonRoundTrip(t, newStore(t)) })
	t.Run("TaskNotFound", func(t *testing.T) { testTaskNotFound(t, newStore(t)) })
	t.Run("TaskRequiresValidWorkspace", func(t *testing.T) { testTaskRequiresValidWorkspace(t, newStore(t)) })
	t.Run("TaskListByWorkspace", func(t *testing.T) { testTaskListByWorkspace(t, newStore(t)) })
	t.Run("TaskListAll", func(t *testing.T) { testTaskListAll(t, newStore(t)) })
	t.Run("TaskSetReapedAtTouchesNothingElse", func(t *testing.T) { testTaskSetReapedAt(t, newStore(t)) })
	t.Run("DeleteWorkspaceWithTasksRejected", func(t *testing.T) { testDeleteWorkspaceWithTasksRejected(t, newStore(t)) })
	t.Run("DeleteTargetWithWorkspacesRejected", func(t *testing.T) { testDeleteTargetWithWorkspacesRejected(t, newStore(t)) })

	t.Run("Message", func(t *testing.T) { testMessageCRUD(t, newStore(t)) })
	t.Run("MessageWithoutTask", func(t *testing.T) { testMessageWithoutTask(t, newStore(t)) })
	t.Run("MessageListByConversationOrdering", func(t *testing.T) { testMessageListByConversationOrdering(t, newStore(t)) })
	t.Run("MessageListByConversationUnknownReturnsEmpty", func(t *testing.T) { testMessageListByConversationUnknownReturnsEmpty(t, newStore(t)) })
	t.Run("MessageRequiresValidTaskWhenSet", func(t *testing.T) { testMessageRequiresValidTaskWhenSet(t, newStore(t)) })
	t.Run("MessageSurvivesTaskDeletion", func(t *testing.T) { testMessageSurvivesTaskDeletion(t, newStore(t)) })
	t.Run("ConversationActivity", func(t *testing.T) { testConversationActivity(t, newStore(t)) })
	t.Run("ConversationActivityEmpty", func(t *testing.T) { testConversationActivityEmpty(t, newStore(t)) })

	t.Run("Dispatch", func(t *testing.T) { testDispatchCRUD(t, newStore(t)) })
	t.Run("DispatchWithUserMessage", func(t *testing.T) { testDispatchWithUserMessage(t, newStore(t)) })
	t.Run("DispatchIdempotencyKeyUnique", func(t *testing.T) { testDispatchIdempotencyKeyUnique(t, newStore(t)) })
	t.Run("DispatchOneActivePerConversation", func(t *testing.T) { testDispatchOneActivePerConversation(t, newStore(t)) })
	t.Run("DispatchTransitionCompareAndSet", func(t *testing.T) { testDispatchTransitionCompareAndSet(t, newStore(t)) })
	t.Run("DispatchListByStatus", func(t *testing.T) { testDispatchListByStatus(t, newStore(t)) })
	t.Run("DispatchNotFound", func(t *testing.T) { testDispatchNotFound(t, newStore(t)) })

	t.Run("Credential", func(t *testing.T) { testCredentialCRUD(t, newStore(t)) })
	t.Run("CredentialNotFound", func(t *testing.T) { testCredentialNotFound(t, newStore(t)) })
	t.Run("CredentialWorkspaceScoped", func(t *testing.T) { testCredentialWorkspaceScoped(t, newStore(t)) })
	t.Run("CredentialRequiresValidWorkspace", func(t *testing.T) { testCredentialRequiresValidWorkspace(t, newStore(t)) })
	t.Run("CredentialListIncludesGlobalAndScoped", func(t *testing.T) { testCredentialListIncludesGlobalAndScoped(t, newStore(t)) })
	t.Run("DeleteWorkspaceWithCredentialRejected", func(t *testing.T) { testDeleteWorkspaceWithCredentialRejected(t, newStore(t)) })

	t.Run("Session", func(t *testing.T) { testSessionCRUD(t, newStore(t)) })
	t.Run("SessionNotFound", func(t *testing.T) { testSessionNotFound(t, newStore(t)) })
	t.Run("SessionTouchUpdatesLastUsedAt", func(t *testing.T) { testSessionTouchUpdatesLastUsedAt(t, newStore(t)) })
	t.Run("SessionListOrderedByLastUsedDescending", func(t *testing.T) { testSessionListOrderedByLastUsedDescending(t, newStore(t)) })
	t.Run("SessionListEmpty", func(t *testing.T) { testSessionListEmpty(t, newStore(t)) })
}

// createTestWorkspace is a fixture: task tests need a valid workspace to
// attach to but aren't testing workspace behavior themselves.
func createTestWorkspace(t *testing.T, s registry.Store) *registry.Workspace {
	t.Helper()
	target := createTestTarget(t, s)
	ws := &registry.Workspace{
		ID:       "fixture-workspace-" + t.Name(),
		Name:     "fixture-workspace-" + t.Name(),
		Path:     "/fixture",
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := s.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}

// createTestTarget is a fixture: most workspace tests need a valid target
// to attach to but aren't testing target behavior themselves.
func createTestTarget(t *testing.T, s registry.Store) *registry.Target {
	t.Helper()
	target := &registry.Target{
		ID:   "fixture-target-" + t.Name(),
		Name: "fixture-target-" + t.Name(),
		Kind: registry.TargetKindLocal,
	}
	if err := s.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}
	return target
}

func testTargetCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()

	target := &registry.Target{
		ID:            "11111111-1111-1111-1111-111111111111",
		Name:          "local",
		Kind:          registry.TargetKindLocal,
		WorkspaceRoot: "/srv/loomux",
	}
	if err := s.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	got, err := s.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if got.WorkspaceRoot != "/srv/loomux" {
		t.Fatalf("WorkspaceRoot = %q, want it persisted", got.WorkspaceRoot)
	}
	if got.Name != target.Name || got.Kind != target.Kind {
		t.Fatalf("GetTarget = %+v, want name=%q kind=%q", got, target.Name, target.Kind)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("GetTarget returned zero timestamps: %+v", got)
	}

	list, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(list) != 1 || list[0].ID != target.ID {
		t.Fatalf("ListTargets = %+v, want single target %q", list, target.ID)
	}

	got.Host = "example.internal"
	if err := s.UpdateTarget(ctx, got); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
	updated, err := s.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTarget after update: %v", err)
	}
	if updated.Host != "example.internal" {
		t.Fatalf("Host = %q after update, want %q", updated.Host, "example.internal")
	}

	if err := s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := s.GetTarget(ctx, target.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTarget after delete: err = %v, want ErrNotFound", err)
	}
}

func testTargetNotFound(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if _, err := s.GetTarget(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTarget: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteTarget(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteTarget: err = %v, want ErrNotFound", err)
	}
}

func testTargetDuplicateName(t *testing.T, s registry.Store) {
	ctx := context.Background()
	a := &registry.Target{ID: "target-a", Name: "dup", Kind: registry.TargetKindLocal}
	b := &registry.Target{ID: "target-b", Name: "dup", Kind: registry.TargetKindLocal}
	if err := s.CreateTarget(ctx, a); err != nil {
		t.Fatalf("CreateTarget(a): %v", err)
	}
	if err := s.CreateTarget(ctx, b); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateTarget(b) with duplicate name: err = %v, want ErrConflict", err)
	}
}

func testWorkspaceCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)

	ws := &registry.Workspace{
		ID:       "workspace-1",
		Name:     "loomux-server",
		Path:     "/home/orski/git/loomux-server",
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := s.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Name != ws.Name || got.Path != ws.Path || got.TargetID != ws.TargetID || got.Status != ws.Status {
		t.Fatalf("GetWorkspace = %+v, want name=%q path=%q target=%q status=%q", got, ws.Name, ws.Path, ws.TargetID, ws.Status)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("GetWorkspace returned zero timestamps: %+v", got)
	}
	if got.RollingSummary != "" {
		t.Fatalf("RollingSummary = %q on creation, want empty", got.RollingSummary)
	}

	byName, err := s.GetWorkspaceByName(ctx, ws.Name)
	if err != nil {
		t.Fatalf("GetWorkspaceByName: %v", err)
	}
	if byName.ID != ws.ID {
		t.Fatalf("GetWorkspaceByName = %+v, want id %q", byName, ws.ID)
	}

	list, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 1 || list[0].ID != ws.ID {
		t.Fatalf("ListWorkspaces = %+v, want single workspace %q", list, ws.ID)
	}

	got.Status = registry.WorkspaceStatusActive
	if err := s.UpdateWorkspace(ctx, got); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	updated, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace after update: %v", err)
	}
	if updated.Status != registry.WorkspaceStatusActive {
		t.Fatalf("Status = %q after update, want %q", updated.Status, registry.WorkspaceStatusActive)
	}

	if err := s.DeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if _, err := s.GetWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetWorkspace after delete: err = %v, want ErrNotFound", err)
	}
}

func testWorkspaceNotFound(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if _, err := s.GetWorkspace(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetWorkspace: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetWorkspaceByName(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetWorkspaceByName: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteWorkspace(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteWorkspace: err = %v, want ErrNotFound", err)
	}
}

func testWorkspaceDuplicateName(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	a := &registry.Workspace{ID: "ws-a", Name: "dup", Path: "/a", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	b := &registry.Workspace{ID: "ws-b", Name: "dup", Path: "/b", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, a); err != nil {
		t.Fatalf("CreateWorkspace(a): %v", err)
	}
	if err := s.CreateWorkspace(ctx, b); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateWorkspace(b) with duplicate name: err = %v, want ErrConflict", err)
	}
}

func testWorkspaceRequiresValidTarget(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := &registry.Workspace{
		ID:       "orphan-workspace",
		Name:     "orphan",
		Path:     "/tmp/orphan",
		TargetID: "does-not-exist",
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := s.CreateWorkspace(ctx, ws); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateWorkspace with bogus target_id: err = %v, want ErrConflict", err)
	}
}

func testWorkspaceRollingSummaryReplaces(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	ws := &registry.Workspace{ID: "ws-summary", Name: "summary-ws", Path: "/x", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	if err := s.SetWorkspaceRollingSummary(ctx, ws.ID, "first summary"); err != nil {
		t.Fatalf("SetWorkspaceRollingSummary(first): %v", err)
	}
	if err := s.SetWorkspaceRollingSummary(ctx, ws.ID, "second summary"); err != nil {
		t.Fatalf("SetWorkspaceRollingSummary(second): %v", err)
	}

	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.RollingSummary != "second summary" {
		t.Fatalf("RollingSummary = %q, want exactly %q (replace, not append)", got.RollingSummary, "second summary")
	}
}

func testWorkspaceTagsAndCapabilitiesRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	ws := &registry.Workspace{
		ID:           "ws-tags",
		Name:         "tagged-ws",
		Path:         "/y",
		TargetID:     target.ID,
		Status:       registry.WorkspaceStatusIdle,
		Tags:         []string{"infra", "k8s"},
		Capabilities: []string{"github-mcp", "kubectl"},
	}
	if err := s.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if !slices.Equal(got.Tags, ws.Tags) {
		t.Fatalf("Tags = %v, want %v", got.Tags, ws.Tags)
	}
	if !slices.Equal(got.Capabilities, ws.Capabilities) {
		t.Fatalf("Capabilities = %v, want %v", got.Capabilities, ws.Capabilities)
	}
}

func testTaskCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)

	task := &registry.Task{
		ID:             "task-1",
		WorkspaceID:    ws.ID,
		Kind:           registry.TaskKindAgent,
		AgentType:      "claude-code",
		TmuxSession:    "loomux-task-1",
		Status:         registry.TaskStatusRunning,
		ConversationID: "conv-1",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.WorkspaceID != task.WorkspaceID || got.Kind != task.Kind || got.AgentType != task.AgentType ||
		got.TmuxSession != task.TmuxSession || got.Status != task.Status || got.ConversationID != task.ConversationID {
		t.Fatalf("GetTask = %+v, want %+v", got, task)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("GetTask returned zero timestamps: %+v", got)
	}
	if got.StartedAt != nil || got.CompletedAt != nil || got.ReapedAt != nil {
		t.Fatalf("GetTask = %+v, want nil StartedAt/CompletedAt/ReapedAt on creation", got)
	}

	got.Status = registry.TaskStatusCompleted
	reapedAt := time.Now().UTC().Truncate(time.Second)
	got.ReapedAt = &reapedAt
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	updated, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if updated.Status != registry.TaskStatusCompleted {
		t.Fatalf("Status = %q after update, want %q", updated.Status, registry.TaskStatusCompleted)
	}
	if updated.ReapedAt == nil || !updated.ReapedAt.Equal(reapedAt) {
		t.Fatalf("ReapedAt = %v after update, want %v", updated.ReapedAt, reapedAt)
	}

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.GetTask(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTask after delete: err = %v, want ErrNotFound", err)
	}
}

func testTaskNotFound(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if _, err := s.GetTask(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTask: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteTask(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteTask: err = %v, want ErrNotFound", err)
	}
}

func testTaskRequiresValidWorkspace(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := &registry.Task{
		ID:             "orphan-task",
		WorkspaceID:    "does-not-exist",
		Kind:           registry.TaskKindShell,
		TmuxSession:    "loomux-orphan",
		Status:         registry.TaskStatusRunning,
		ConversationID: "conv-x",
	}
	if err := s.CreateTask(ctx, task); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateTask with bogus workspace_id: err = %v, want ErrConflict", err)
	}
}

func testTaskListByWorkspace(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)

	wsA := &registry.Workspace{ID: "ws-a-" + t.Name(), Name: "ws-a-" + t.Name(), Path: "/a", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, wsA); err != nil {
		t.Fatalf("CreateWorkspace(wsA): %v", err)
	}
	wsB := &registry.Workspace{ID: "ws-b-" + t.Name(), Name: "ws-b-" + t.Name(), Path: "/b", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, wsB); err != nil {
		t.Fatalf("CreateWorkspace(wsB): %v", err)
	}

	mk := func(id, workspaceID string) *registry.Task {
		return &registry.Task{
			ID: id, WorkspaceID: workspaceID, Kind: registry.TaskKindShell,
			TmuxSession: "sess-" + id, Status: registry.TaskStatusRunning, ConversationID: "conv",
		}
	}
	if err := s.CreateTask(ctx, mk("task-a1", wsA.ID)); err != nil {
		t.Fatalf("CreateTask(a1): %v", err)
	}
	if err := s.CreateTask(ctx, mk("task-a2", wsA.ID)); err != nil {
		t.Fatalf("CreateTask(a2): %v", err)
	}
	if err := s.CreateTask(ctx, mk("task-b1", wsB.ID)); err != nil {
		t.Fatalf("CreateTask(b1): %v", err)
	}

	list, err := s.ListTasksByWorkspace(ctx, wsA.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTasksByWorkspace(wsA) = %d tasks, want 2", len(list))
	}
	for _, task := range list {
		if task.WorkspaceID != wsA.ID {
			t.Fatalf("ListTasksByWorkspace(wsA) returned task for workspace %q", task.WorkspaceID)
		}
	}
}

// testTaskListAll covers ListTasks — the unfiltered, cross-workspace query
// LOOM-18's conversation-listing endpoints are built on, since a
// conversation isn't pinned to one workspace (the router can route the
// same conversation_id to a different workspace on a later message).
func testTaskListAll(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)

	wsA := &registry.Workspace{ID: "ws-a-" + t.Name(), Name: "ws-a-" + t.Name(), Path: "/a", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, wsA); err != nil {
		t.Fatalf("CreateWorkspace(wsA): %v", err)
	}
	wsB := &registry.Workspace{ID: "ws-b-" + t.Name(), Name: "ws-b-" + t.Name(), Path: "/b", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, wsB); err != nil {
		t.Fatalf("CreateWorkspace(wsB): %v", err)
	}

	mk := func(id, workspaceID string) *registry.Task {
		return &registry.Task{
			ID: id, WorkspaceID: workspaceID, Kind: registry.TaskKindShell,
			TmuxSession: "sess-" + id, Status: registry.TaskStatusRunning, ConversationID: "conv",
		}
	}
	if err := s.CreateTask(ctx, mk("task-a1", wsA.ID)); err != nil {
		t.Fatalf("CreateTask(a1): %v", err)
	}
	if err := s.CreateTask(ctx, mk("task-b1", wsB.ID)); err != nil {
		t.Fatalf("CreateTask(b1): %v", err)
	}

	list, err := s.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTasks = %d tasks, want 2 (across both workspaces)", len(list))
	}
	seen := map[string]bool{}
	for _, task := range list {
		seen[task.ID] = true
	}
	if !seen["task-a1"] || !seen["task-b1"] {
		t.Fatalf("ListTasks = %+v, want both task-a1 (wsA) and task-b1 (wsB)", list)
	}
}

func testDeleteWorkspaceWithTasksRejected(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID: "blocking-task", WorkspaceID: ws.ID, Kind: registry.TaskKindShell,
		TmuxSession: "sess", Status: registry.TaskStatusRunning, ConversationID: "conv",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace with a task attached: err = %v, want ErrConflict", err)
	}
}

func testDeleteTargetWithWorkspacesRejected(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	ws := &registry.Workspace{ID: "blocking-ws", Name: "blocking-ws", Path: "/z", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.DeleteTarget(ctx, target.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteTarget with a workspace attached: err = %v, want ErrConflict", err)
	}
}

func testMessageCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID: "msg-task-" + t.Name(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent,
		AgentType: "claude-code", TmuxSession: "sess-" + t.Name(), Status: registry.TaskStatusRunning,
		ConversationID: "conv-1",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	msg := &registry.Message{
		ID:             "msg-1-" + t.Name(),
		ConversationID: "conv-1",
		TaskID:         task.ID,
		Role:           registry.MessageRoleUser,
		Content:        "hello there",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if msg.CreatedAt.IsZero() {
		t.Fatalf("CreateMessage did not set CreatedAt: %+v", msg)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListMessagesByConversation = %+v, want 1 message", list)
	}
	got := list[0]
	if got.ID != msg.ID || got.ConversationID != "conv-1" || got.TaskID != task.ID ||
		got.Role != registry.MessageRoleUser || got.Content != "hello there" {
		t.Fatalf("ListMessagesByConversation[0] = %+v, want %+v", got, msg)
	}
}

func testMessageWithoutTask(t *testing.T, s registry.Store) {
	ctx := context.Background()
	msg := &registry.Message{
		ID:             "msg-direct-" + t.Name(),
		ConversationID: "conv-direct",
		Role:           registry.MessageRoleAssistant,
		Content:        "a direct answer",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage (no task): %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-direct")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 || list[0].TaskID != "" {
		t.Fatalf("ListMessagesByConversation = %+v, want 1 message with empty TaskID", list)
	}
}

// testMessageListByConversationOrdering deliberately inserts three
// messages back-to-back with no artificial delay between them, to prove
// ordering is guaranteed by insertion order and doesn't depend on
// created_at having enough timestamp resolution to distinguish rows
// written in the same instant.
func testMessageListByConversationOrdering(t *testing.T, s registry.Store) {
	ctx := context.Background()
	mk := func(id, conversationID, content string) *registry.Message {
		return &registry.Message{ID: id, ConversationID: conversationID, Role: registry.MessageRoleUser, Content: content}
	}
	if err := s.CreateMessage(ctx, mk("m1", "conv-order", "first")); err != nil {
		t.Fatalf("CreateMessage(m1): %v", err)
	}
	if err := s.CreateMessage(ctx, mk("m2", "conv-order", "second")); err != nil {
		t.Fatalf("CreateMessage(m2): %v", err)
	}
	if err := s.CreateMessage(ctx, mk("m3", "conv-order", "third")); err != nil {
		t.Fatalf("CreateMessage(m3): %v", err)
	}
	// An unrelated conversation must not leak in.
	if err := s.CreateMessage(ctx, mk("m-other", "conv-other", "unrelated")); err != nil {
		t.Fatalf("CreateMessage(m-other): %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-order")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListMessagesByConversation = %+v, want 3 messages", list)
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if list[i].Content != w {
			t.Fatalf("ListMessagesByConversation[%d].Content = %q, want %q (insertion order = %+v)", i, list[i].Content, w, list)
		}
	}
}

func testMessageListByConversationUnknownReturnsEmpty(t *testing.T, s registry.Store) {
	list, err := s.ListMessagesByConversation(context.Background(), "no-such-conversation")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListMessagesByConversation(unknown) = %+v, want empty", list)
	}
}

func testMessageRequiresValidTaskWhenSet(t *testing.T, s registry.Store) {
	ctx := context.Background()
	msg := &registry.Message{
		ID: "msg-bad-task", ConversationID: "conv-1", TaskID: "does-not-exist",
		Role: registry.MessageRoleUser, Content: "hi",
	}
	if err := s.CreateMessage(ctx, msg); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateMessage with an unknown task id: err = %v, want ErrConflict", err)
	}
}

func testMessageSurvivesTaskDeletion(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID: "msg-del-task", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent,
		AgentType: "claude-code", TmuxSession: "sess-del", Status: registry.TaskStatusCompleted,
		ConversationID: "conv-del",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	msg := &registry.Message{
		ID: "msg-del-1", ConversationID: "conv-del", TaskID: task.ID,
		Role: registry.MessageRoleUser, Content: "hi",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-del")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListMessagesByConversation = %+v, want the message to survive task deletion", list)
	}
	if list[0].TaskID != "" {
		t.Fatalf("message TaskID = %q after its task was deleted, want empty (ON DELETE SET NULL)", list[0].TaskID)
	}
}

func testCredentialCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()

	cred := &registry.Credential{
		ID:    "cred-1",
		Name:  "GITHUB_TOKEN",
		Value: "super-secret-value",
	}
	if err := s.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	got, err := s.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if got.Name != cred.Name || got.Value != cred.Value {
		t.Fatalf("GetCredential = %+v, want name=%q value=%q", got, cred.Name, cred.Value)
	}
	if got.WorkspaceID != "" || got.AgentType != "" {
		t.Fatalf("GetCredential = %+v, want unscoped (empty WorkspaceID/AgentType)", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("GetCredential returned zero timestamps: %+v", got)
	}

	if err := s.DeleteCredential(ctx, cred.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := s.GetCredential(ctx, cred.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetCredential after delete: err = %v, want ErrNotFound", err)
	}
}

func testCredentialNotFound(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if _, err := s.GetCredential(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetCredential: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCredential(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteCredential: err = %v, want ErrNotFound", err)
	}
}

func testCredentialWorkspaceScoped(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)

	cred := &registry.Credential{
		ID:          "cred-scoped",
		Name:        "GITHUB_TOKEN",
		WorkspaceID: ws.ID,
		AgentType:   "claude-code",
		Value:       "scoped-value",
	}
	if err := s.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	got, err := s.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if got.WorkspaceID != ws.ID || got.AgentType != "claude-code" || got.Value != "scoped-value" {
		t.Fatalf("GetCredential = %+v, want workspace=%q agentType=%q value=%q", got, ws.ID, "claude-code", "scoped-value")
	}
}

func testCredentialRequiresValidWorkspace(t *testing.T, s registry.Store) {
	ctx := context.Background()
	cred := &registry.Credential{
		ID:          "orphan-cred",
		Name:        "GITHUB_TOKEN",
		WorkspaceID: "does-not-exist",
		Value:       "value",
	}
	if err := s.CreateCredential(ctx, cred); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateCredential with bogus workspace_id: err = %v, want ErrConflict", err)
	}
}

func testCredentialListIncludesGlobalAndScoped(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)

	global := &registry.Credential{ID: "cred-global", Name: "GLOBAL_TOKEN", Value: "g"}
	wsScoped := &registry.Credential{ID: "cred-ws", Name: "WS_TOKEN", WorkspaceID: ws.ID, Value: "w"}
	agentScoped := &registry.Credential{ID: "cred-agent", Name: "AGENT_TOKEN", AgentType: "claude-code", Value: "a"}
	for _, c := range []*registry.Credential{global, wsScoped, agentScoped} {
		if err := s.CreateCredential(ctx, c); err != nil {
			t.Fatalf("CreateCredential(%s): %v", c.ID, err)
		}
	}

	list, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListCredentials = %d credentials, want 3", len(list))
	}
}

func testDeleteWorkspaceWithCredentialRejected(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	cred := &registry.Credential{ID: "blocking-cred", Name: "TOKEN", WorkspaceID: ws.ID, Value: "v"}
	if err := s.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := s.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace with a credential attached: err = %v, want ErrConflict", err)
	}
}

func testSessionCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	lastUsed := time.Now().UTC().Truncate(time.Second)

	sess := &registry.Session{ID: "sess-1", TokenHash: "hash-1", LastUsedAt: lastUsed}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSessionByTokenHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if got.ID != sess.ID || got.TokenHash != "hash-1" {
		t.Fatalf("GetSessionByTokenHash = %+v, want id=%q tokenHash=%q", got, sess.ID, "hash-1")
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("GetSessionByTokenHash returned zero CreatedAt: %+v", got)
	}
	if !got.LastUsedAt.Equal(lastUsed) {
		t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, lastUsed)
	}

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSessionByTokenHash(ctx, "hash-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetSessionByTokenHash after delete: err = %v, want ErrNotFound", err)
	}
}

func testSessionNotFound(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if _, err := s.GetSessionByTokenHash(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetSessionByTokenHash: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, "does-not-exist"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteSession: err = %v, want ErrNotFound", err)
	}
}

func testSessionTouchUpdatesLastUsedAt(t *testing.T, s registry.Store) {
	ctx := context.Background()
	sess := &registry.Session{ID: "sess-touch", TokenHash: "hash-touch", LastUsedAt: time.Now().UTC().Add(-time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	newTime := time.Now().UTC().Truncate(time.Second)
	if err := s.TouchSession(ctx, sess.ID, newTime); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}

	got, err := s.GetSessionByTokenHash(ctx, "hash-touch")
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if !got.LastUsedAt.Equal(newTime) {
		t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, newTime)
	}

	if err := s.TouchSession(ctx, "does-not-exist", newTime); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("TouchSession on unknown id: err = %v, want ErrNotFound", err)
	}
}

// testSessionListOrderedByLastUsedDescending (LOOM-47) proves
// ListSessions returns every session, most-recently-used first — the
// order a "revoke a device" UI needs (most likely to still matter to
// the caller listed first).
func testSessionListOrderedByLastUsedDescending(t *testing.T, s registry.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	oldest := &registry.Session{ID: "sess-oldest", TokenHash: "hash-oldest", LastUsedAt: now.Add(-2 * time.Hour)}
	middle := &registry.Session{ID: "sess-middle", TokenHash: "hash-middle", LastUsedAt: now.Add(-1 * time.Hour)}
	newest := &registry.Session{ID: "sess-newest", TokenHash: "hash-newest", LastUsedAt: now}
	for _, sess := range []*registry.Session{oldest, middle, newest} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession(%s): %v", sess.ID, err)
		}
	}

	got, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListSessions returned %d sessions, want 3", len(got))
	}
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	wantIDs := []string{"sess-newest", "sess-middle", "sess-oldest"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ListSessions order = %v, want %v (most-recently-used first)", gotIDs, wantIDs)
	}
}

// testSessionListEmpty proves ListSessions on a fresh store returns an
// empty result, not an error — mirroring every other List* method's own
// "nothing yet" stance elsewhere in this suite.
func testSessionListEmpty(t *testing.T, s registry.Store) {
	got, err := s.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSessions on empty store = %+v, want empty", got)
	}
}

// testConversationActivity: ListConversationActivity reports one row per
// conversation that has any message — whether or not it ever touched a
// task (LOOM-62: answer_directly-only conversations have none) — with
// its latest message's created_at.
func testConversationActivity(t *testing.T, s registry.Store) {
	ctx := context.Background()
	mk := func(id, conversationID string) {
		t.Helper()
		m := &registry.Message{ID: id, ConversationID: conversationID, Role: registry.MessageRoleUser, Content: id}
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage(%s): %v", id, err)
		}
	}
	mk("a1", "conv-a")
	time.Sleep(5 * time.Millisecond)
	mk("b1", "conv-b")
	time.Sleep(5 * time.Millisecond)
	mk("a2", "conv-a")

	latest := func(conversationID string) time.Time {
		t.Helper()
		msgs, err := s.ListMessagesByConversation(ctx, conversationID)
		if err != nil || len(msgs) == 0 {
			t.Fatalf("ListMessagesByConversation(%s) = %v, %v", conversationID, msgs, err)
		}
		return msgs[len(msgs)-1].CreatedAt
	}
	want := map[string]time.Time{"conv-a": latest("conv-a"), "conv-b": latest("conv-b")}

	got, err := s.ListConversationActivity(ctx)
	if err != nil {
		t.Fatalf("ListConversationActivity: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListConversationActivity = %+v, want one row each for %v", got, want)
	}
	for _, a := range got {
		w, ok := want[a.ConversationID]
		if !ok {
			t.Fatalf("unexpected conversation %q", a.ConversationID)
		}
		if !a.LastMessageAt.Equal(w) {
			t.Errorf("%s LastMessageAt = %v, want %v (its latest message)", a.ConversationID, a.LastMessageAt, w)
		}
	}
}

func testConversationActivityEmpty(t *testing.T, s registry.Store) {
	got, err := s.ListConversationActivity(context.Background())
	if err != nil {
		t.Fatalf("ListConversationActivity: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("ListConversationActivity = %#v, want an empty non-nil slice", got)
	}
}

func testTargetAgentUpsertAndList(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)

	got, err := s.ListTargetAgents(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListTargetAgents (never probed): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListTargetAgents (never probed) = %v, want empty", got)
	}

	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, a := range []*registry.TargetAgent{
		{TargetID: target.ID, AgentType: "codex", Available: false, CheckedAt: first},
		{TargetID: target.ID, AgentType: "claude-code", Available: true, CheckedAt: first},
	} {
		if err := s.SetTargetAgent(ctx, a); err != nil {
			t.Fatalf("SetTargetAgent(%s): %v", a.AgentType, err)
		}
	}
	// A later probe replaces the earlier result rather than adding a row.
	second := first.Add(30 * time.Minute)
	if err := s.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "codex", Available: true, CheckedAt: second}); err != nil {
		t.Fatalf("SetTargetAgent(codex again): %v", err)
	}

	got, err = s.ListTargetAgents(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListTargetAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTargetAgents = %d rows, want 2", len(got))
	}
	if got[0].AgentType != "claude-code" || !got[0].Available || !got[0].CheckedAt.Equal(first) {
		t.Errorf("row 0 = %+v, want claude-code available at %v", got[0], first)
	}
	if got[1].AgentType != "codex" || !got[1].Available || !got[1].CheckedAt.Equal(second) {
		t.Errorf("row 1 = %+v, want codex available at %v (the newer probe)", got[1], second)
	}
}

func testTargetAgentRequiresValidTarget(t *testing.T, s registry.Store) {
	err := s.SetTargetAgent(context.Background(), &registry.TargetAgent{TargetID: "no-such-target", AgentType: "codex"})
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("SetTargetAgent on unknown target: err = %v, want ErrConflict", err)
	}
}

func testTargetAgentDeletedWithTarget(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	if err := s.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "codex", Available: true}); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
	if err := s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget with only probe results: %v", err)
	}
	got, err := s.ListTargetAgents(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListTargetAgents after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("probe results outlived their target: %v", got)
	}
}

func testTargetAgentAuthStatus(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	if err := s.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "claude", Available: true,
		Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedOut}); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
	got, err := s.ListTargetAgents(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListTargetAgents: %v", err)
	}
	if len(got) != 1 || got[0].AuthStatus != registry.AgentAuthLoggedOut {
		t.Fatalf("ListTargetAgents = %+v, want one row logged out", got)
	}
}

func testTargetHealthRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	if _, err := s.GetTargetHealth(ctx, target.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTargetHealth (never probed): err = %v, want ErrNotFound", err)
	}

	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	want := &registry.TargetHealth{TargetID: target.ID, Reachable: true, Latency: 250 * time.Millisecond,
		TmuxVersion: "tmux 3.4", DiskFreeBytes: 5 << 30, ProbedAt: first}
	if err := s.SetTargetHealth(ctx, want); err != nil {
		t.Fatalf("SetTargetHealth: %v", err)
	}
	got, err := s.GetTargetHealth(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTargetHealth: %v", err)
	}
	if !got.Reachable || got.Latency != want.Latency || got.TmuxVersion != want.TmuxVersion ||
		got.DiskFreeBytes != want.DiskFreeBytes || !got.ProbedAt.Equal(first) || got.Error != "" {
		t.Errorf("GetTargetHealth = %+v, want %+v", got, want)
	}

	// A later probe replaces the earlier one.
	second := first.Add(5 * time.Minute)
	if err := s.SetTargetHealth(ctx, &registry.TargetHealth{TargetID: target.ID, Reachable: false,
		Error: "no answer", DiskFreeBytes: -1, ProbedAt: second}); err != nil {
		t.Fatalf("SetTargetHealth (again): %v", err)
	}
	got, err = s.GetTargetHealth(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTargetHealth: %v", err)
	}
	if got.Reachable || got.Error != "no answer" || got.DiskFreeBytes != -1 || got.TmuxVersion != "" || !got.ProbedAt.Equal(second) {
		t.Errorf("GetTargetHealth after a second probe = %+v", got)
	}
}

func testTargetHealthRequiresValidTarget(t *testing.T, s registry.Store) {
	err := s.SetTargetHealth(context.Background(), &registry.TargetHealth{TargetID: "no-such-target"})
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("SetTargetHealth on unknown target: err = %v, want ErrConflict", err)
	}
}

func testTargetHealthDeletedWithTarget(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	if err := s.SetTargetHealth(ctx, &registry.TargetHealth{TargetID: target.ID, Reachable: true}); err != nil {
		t.Fatalf("SetTargetHealth: %v", err)
	}
	if err := s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget with a health record: %v", err)
	}
	if _, err := s.GetTargetHealth(ctx, target.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("health record outlived its target: err = %v", err)
	}
}

func testWorkspaceStatusFailed(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	ws.Status = registry.WorkspaceStatusFailed
	if err := s.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace(status failed): %v", err)
	}
	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Status != registry.WorkspaceStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
}

func testCommandTaskRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID:             "command-task",
		WorkspaceID:    ws.ID,
		Kind:           registry.TaskKindCommand,
		TmuxSession:    "loomux-command",
		Status:         registry.TaskStatusRunning,
		ConversationID: "conv",
		Command:        "hostname && uptime",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Kind != registry.TaskKindCommand || got.Command != "hostname && uptime" || got.ExitCode != nil {
		t.Errorf("created task = %+v, want command kind, command recorded, no exit code yet", got)
	}

	code := 127
	got.ExitCode = &code
	got.Status = registry.TaskStatusFailed
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	got, err = s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if got.ExitCode == nil || *got.ExitCode != 127 {
		t.Errorf("ExitCode = %v, want 127", got.ExitCode)
	}
}

func testTaskFailureRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{ID: "failing", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "codex",
		TmuxSession: "sess", Status: registry.TaskStatusRunning, ConversationID: "c"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = registry.TaskStatusFailed
	task.FailureReason = "agent exited with status 127"
	task.ErrorClass = registry.ErrorClassAgentExited
	task.OutputTail = "sh: 1: codex: not found"
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != task.FailureReason || got.ErrorClass != registry.ErrorClassAgentExited || got.OutputTail != task.OutputTail {
		t.Errorf("failure after round trip = %q / %q / %q", got.FailureReason, got.ErrorClass, got.OutputTail)
	}
}

func testTaskAttentionRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{ID: "asking", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "sess", Status: registry.TaskStatusRunning, ConversationID: "c"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, err := s.GetTask(ctx, task.ID); err != nil || got.Attention != nil {
		t.Fatalf("fresh task: Attention = %+v, err %v; want nil", got.Attention, err)
	}
	task.Status = registry.TaskStatusNeedsAttention
	task.Attention = &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command",
		Detail: "touch a.txt", Question: "Do you want to proceed?",
		Options: []registry.AttentionOption{{Label: "Yes"}, {Label: "No", Description: "stop"}}, Selected: 1}
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != registry.TaskStatusNeedsAttention || !reflect.DeepEqual(got.Attention, task.Attention) {
		t.Errorf("after round trip: status %q, attention %+v; want %+v", got.Status, got.Attention, task.Attention)
	}
	task.Status, task.Attention = registry.TaskStatusRunning, nil
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if got, err := s.GetTask(ctx, task.ID); err != nil || got.Attention != nil {
		t.Errorf("cleared: Attention = %+v, err %v; want nil", got.Attention, err)
	}
}

func testWorkspaceStatusReasonRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	ws.Status = registry.WorkspaceStatusFailed
	ws.StatusReason = "provisioning command exited with status 3"
	if err := s.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.StatusReason != ws.StatusReason {
		t.Errorf("StatusReason = %q, want %q", got.StatusReason, ws.StatusReason)
	}
}

func testTargetAgentPathAndVersion(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	if err := s.SetTargetAgent(ctx, &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code", Available: true,
		Path: "/home/u/.local/bin/claude", Version: "2.1.251 (Claude Code)"}); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
	got, err := s.ListTargetAgents(ctx, target.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListTargetAgents = %v, %v", got, err)
	}
	if got[0].Path != "/home/u/.local/bin/claude" || got[0].Version != "2.1.251 (Claude Code)" {
		t.Errorf("path/version = %q / %q", got[0].Path, got[0].Version)
	}
}

func newTestDispatch(id, conversationID string) *registry.Dispatch {
	return &registry.Dispatch{
		ID: id, ConversationID: conversationID, Message: "hello", WorkspaceHint: "ws-hint",
		RequestHash: "hash-" + id, Status: registry.DispatchStatusQueued,
	}
}

func testDispatchCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	d := newTestDispatch("d-1", "conv-d")
	d.IdempotencyKey = "key-1"
	if err := s.CreateDispatch(ctx, d, nil); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		t.Fatalf("CreateDispatch did not stamp times: %+v", d)
	}
	got, err := s.GetDispatch(ctx, "d-1")
	if err != nil {
		t.Fatalf("GetDispatch: %v", err)
	}
	if got.ConversationID != "conv-d" || got.Message != "hello" || got.WorkspaceHint != "ws-hint" ||
		got.IdempotencyKey != "key-1" || got.RequestHash != "hash-d-1" || got.Status != registry.DispatchStatusQueued ||
		got.StartedAt != nil || got.FinishedAt != nil {
		t.Fatalf("GetDispatch = %+v", got)
	}
	byKey, err := s.GetDispatchByIdempotencyKey(ctx, "key-1")
	if err != nil || byKey.ID != "d-1" {
		t.Fatalf("GetDispatchByIdempotencyKey = %+v, %v", byKey, err)
	}

	started := time.Now().UTC()
	got.Status = registry.DispatchStatusRunning
	got.StartedAt = &started
	if err := s.TransitionDispatch(ctx, got, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch queued→running: %v", err)
	}
	finished := time.Now().UTC()
	got.Status = registry.DispatchStatusFailed
	got.Error = "boom"
	got.ErrorClass = registry.ErrorClassTimeout
	got.FinishedAt = &finished
	if err := s.TransitionDispatch(ctx, got, registry.DispatchStatusRunning); err != nil {
		t.Fatalf("TransitionDispatch running→failed: %v", err)
	}
	list, err := s.ListDispatchesByConversation(ctx, "conv-d")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDispatchesByConversation = %+v, %v", list, err)
	}
	if l := list[0]; l.Status != registry.DispatchStatusFailed || l.Error != "boom" || l.ErrorClass != registry.ErrorClassTimeout ||
		l.StartedAt == nil || l.FinishedAt == nil {
		t.Fatalf("after transitions = %+v", l)
	}
	empty, err := s.ListDispatchesByConversation(ctx, "conv-unknown")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListDispatchesByConversation(unknown) = %#v, %v; want empty, non-nil", empty, err)
	}
}

func testDispatchWithUserMessage(t *testing.T, s registry.Store) {
	ctx := context.Background()
	d := newTestDispatch("d-msg", "conv-msg")
	msg := &registry.Message{ID: "m-user", ConversationID: "conv-msg", Role: registry.MessageRoleUser, Content: "hello"}
	if err := s.CreateDispatch(ctx, d, msg); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	msgs, err := s.ListMessagesByConversation(ctx, "conv-msg")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessagesByConversation = %+v, %v", msgs, err)
	}
	if msgs[0].DispatchID != "d-msg" || msgs[0].Content != "hello" {
		t.Fatalf("user message = %+v, want dispatch_id d-msg", msgs[0])
	}

	// A failing dispatch insert must not leave its message behind.
	dup := newTestDispatch("d-msg", "conv-msg-2")
	orphan := &registry.Message{ID: "m-orphan", ConversationID: "conv-msg-2", Role: registry.MessageRoleUser, Content: "x"}
	if err := s.CreateDispatch(ctx, dup, orphan); err == nil {
		t.Fatalf("CreateDispatch with duplicate id succeeded")
	}
	if msgs, _ := s.ListMessagesByConversation(ctx, "conv-msg-2"); len(msgs) != 0 {
		t.Fatalf("message written despite failed dispatch insert: %+v", msgs)
	}

	// A message with a dispatch id round-trips through CreateMessage too.
	reply := &registry.Message{ID: "m-reply", ConversationID: "conv-msg", DispatchID: "d-msg", Role: registry.MessageRoleAssistant, Content: "hi"}
	if err := s.CreateMessage(ctx, reply); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	msgs, _ = s.ListMessagesByConversation(ctx, "conv-msg")
	if len(msgs) != 2 || msgs[1].DispatchID != "d-msg" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func testDispatchIdempotencyKeyUnique(t *testing.T, s registry.Store) {
	ctx := context.Background()
	a := newTestDispatch("d-a", "conv-a")
	a.IdempotencyKey = "same"
	if err := s.CreateDispatch(ctx, a, nil); err != nil {
		t.Fatalf("CreateDispatch a: %v", err)
	}
	b := newTestDispatch("d-b", "conv-b")
	b.IdempotencyKey = "same"
	if err := s.CreateDispatch(ctx, b, nil); !errors.Is(err, registry.ErrIdempotencyKeyExists) {
		t.Fatalf("CreateDispatch with reused key = %v, want ErrIdempotencyKeyExists", err)
	}
	// No key at all is never a conflict.
	c, d := newTestDispatch("d-c", "conv-c"), newTestDispatch("d-d", "conv-d2")
	if err := s.CreateDispatch(ctx, c, nil); err != nil {
		t.Fatalf("CreateDispatch c: %v", err)
	}
	if err := s.CreateDispatch(ctx, d, nil); err != nil {
		t.Fatalf("CreateDispatch d: %v", err)
	}
	if _, err := s.GetDispatchByIdempotencyKey(ctx, "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetDispatchByIdempotencyKey(unknown) = %v, want ErrNotFound", err)
	}
}

func testDispatchOneActivePerConversation(t *testing.T, s registry.Store) {
	ctx := context.Background()
	first := newTestDispatch("d-first", "conv-busy")
	if err := s.CreateDispatch(ctx, first, nil); err != nil {
		t.Fatalf("CreateDispatch first: %v", err)
	}
	second := newTestDispatch("d-second", "conv-busy")
	if err := s.CreateDispatch(ctx, second, nil); !errors.Is(err, registry.ErrConversationBusy) {
		t.Fatalf("CreateDispatch while queued = %v, want ErrConversationBusy", err)
	}
	first.Status = registry.DispatchStatusRunning
	if err := s.TransitionDispatch(ctx, first, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
	if err := s.CreateDispatch(ctx, second, nil); !errors.Is(err, registry.ErrConversationBusy) {
		t.Fatalf("CreateDispatch while running = %v, want ErrConversationBusy", err)
	}
	first.Status = registry.DispatchStatusSucceeded
	if err := s.TransitionDispatch(ctx, first, registry.DispatchStatusRunning); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
	if err := s.CreateDispatch(ctx, second, nil); err != nil {
		t.Fatalf("CreateDispatch after the first finished: %v", err)
	}
}

func testDispatchTransitionCompareAndSet(t *testing.T, s registry.Store) {
	ctx := context.Background()
	d := newTestDispatch("d-cas", "conv-cas")
	if err := s.CreateDispatch(ctx, d, nil); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	d.Status = registry.DispatchStatusInterrupted
	if err := s.TransitionDispatch(ctx, d, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
	late := *d
	late.Status = registry.DispatchStatusSucceeded
	late.Reply = "too late"
	if err := s.TransitionDispatch(ctx, &late, registry.DispatchStatusRunning); !errors.Is(err, registry.ErrDispatchStateChanged) {
		t.Fatalf("stale TransitionDispatch = %v, want ErrDispatchStateChanged", err)
	}
	got, _ := s.GetDispatch(ctx, "d-cas")
	if got.Status != registry.DispatchStatusInterrupted || got.Reply != "" {
		t.Fatalf("stale transition overwrote the row: %+v", got)
	}
	ghost := newTestDispatch("d-ghost", "conv-ghost")
	if err := s.TransitionDispatch(ctx, ghost, registry.DispatchStatusQueued); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("TransitionDispatch(unknown) = %v, want ErrNotFound", err)
	}
}

func testDispatchListByStatus(t *testing.T, s registry.Store) {
	ctx := context.Background()
	for _, id := range []string{"q1", "r1", "s1"} {
		if err := s.CreateDispatch(ctx, newTestDispatch(id, "conv-"+id), nil); err != nil {
			t.Fatalf("CreateDispatch %s: %v", id, err)
		}
	}
	r1, _ := s.GetDispatch(ctx, "r1")
	r1.Status = registry.DispatchStatusRunning
	if err := s.TransitionDispatch(ctx, r1, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
	s1, _ := s.GetDispatch(ctx, "s1")
	s1.Status = registry.DispatchStatusSucceeded
	if err := s.TransitionDispatch(ctx, s1, registry.DispatchStatusQueued); err != nil {
		t.Fatalf("TransitionDispatch: %v", err)
	}
	active, err := s.ListDispatchesByStatus(ctx, registry.DispatchStatusQueued, registry.DispatchStatusRunning)
	if err != nil {
		t.Fatalf("ListDispatchesByStatus: %v", err)
	}
	var ids []string
	for _, d := range active {
		ids = append(ids, d.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"q1", "r1"}) {
		t.Fatalf("ListDispatchesByStatus(queued, running) = %v", ids)
	}
}

func testDispatchNotFound(t *testing.T, s registry.Store) {
	if _, err := s.GetDispatch(context.Background(), "missing"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetDispatch(missing) = %v, want ErrNotFound", err)
	}
}

func testTaskSetReapedAt(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{ID: "reap-" + t.Name(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "s", Status: registry.TaskStatusFailed, ConversationID: "c", FailureReason: "gone", ErrorClass: registry.ErrorClassSessionLost}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.SetTaskReapedAt(ctx, task.ID, at); err != nil {
		t.Fatalf("SetTaskReapedAt: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ReapedAt == nil || !got.ReapedAt.Equal(at) {
		t.Errorf("ReapedAt = %v, want %v", got.ReapedAt, at)
	}
	if got.Status != registry.TaskStatusFailed || got.FailureReason != "gone" || got.ErrorClass != registry.ErrorClassSessionLost {
		t.Errorf("other fields changed: %+v", got)
	}
	if err := s.SetTaskReapedAt(ctx, "missing", at); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("SetTaskReapedAt(missing) = %v, want ErrNotFound", err)
	}
}

func testTaskTurns(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{ID: "turns-task", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-turns", Status: registry.TaskStatusRunning, ConversationID: "conv"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, err := s.ListTaskTurns(ctx, task.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ListTaskTurns (none) = %v, %v; want an empty slice", got, err)
	}
	for i, msg := range []string{"first", "second"} {
		turn := &registry.TaskTurn{ID: fmt.Sprintf("turn-%d", i), TaskID: task.ID, UserMessage: msg,
			AgentMessage: "answer to " + msg, Pane: "screen " + msg}
		if err := s.CreateTaskTurn(ctx, turn); err != nil {
			t.Fatalf("CreateTaskTurn: %v", err)
		}
		if turn.CreatedAt.IsZero() {
			t.Error("CreateTaskTurn did not stamp CreatedAt")
		}
	}
	got, err := s.ListTaskTurns(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskTurns: %v", err)
	}
	if len(got) != 2 || got[0].UserMessage != "first" || got[1].AgentMessage != "answer to second" || got[1].Pane != "screen second" {
		t.Fatalf("ListTaskTurns = %+v", got)
	}
	if err := s.CreateTaskTurn(ctx, &registry.TaskTurn{ID: "orphan", TaskID: "no-such-task"}); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("CreateTaskTurn on unknown task: err = %v, want ErrConflict", err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("DeleteTask with turns: %v", err)
	}
	if got, _ := s.ListTaskTurns(ctx, task.ID); len(got) != 0 {
		t.Errorf("turns outlived their task: %+v", got)
	}
}
