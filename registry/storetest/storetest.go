// Package storetest is the backend-conformance test suite for
// registry.Store. Any backend implementation should pass Run unmodified —
// that's what makes "adding a new backend means passing the existing
// suite" (design spec, Testing Strategy) literally true.
package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Loomux/server/registry"
)

// Run executes the full conformance suite. newStore must return a fresh,
// empty Store for each call; the caller is responsible for its cleanup
// (e.g. via t.Cleanup).
func Run(t *testing.T, newStore func(t *testing.T) registry.Store) {
	t.Run("Target", func(t *testing.T) { testTargetCRUD(t, newStore(t)) })
	t.Run("TargetNotFound", func(t *testing.T) { testTargetNotFound(t, newStore(t)) })
	t.Run("TargetDuplicateName", func(t *testing.T) { testTargetDuplicateName(t, newStore(t)) })

	t.Run("Workspace", func(t *testing.T) { testWorkspaceCRUD(t, newStore(t)) })
	t.Run("WorkspaceNotFound", func(t *testing.T) { testWorkspaceNotFound(t, newStore(t)) })
	t.Run("WorkspaceDuplicateName", func(t *testing.T) { testWorkspaceDuplicateName(t, newStore(t)) })
	t.Run("WorkspaceRequiresValidTarget", func(t *testing.T) { testWorkspaceRequiresValidTarget(t, newStore(t)) })
	t.Run("WorkspaceRollingSummaryReplaces", func(t *testing.T) { testWorkspaceRollingSummaryReplaces(t, newStore(t)) })
	t.Run("WorkspaceTagsAndCapabilitiesRoundTrip", func(t *testing.T) { testWorkspaceTagsAndCapabilitiesRoundTrip(t, newStore(t)) })

	t.Run("Task", func(t *testing.T) { testTaskCRUD(t, newStore(t)) })
	t.Run("TaskNotFound", func(t *testing.T) { testTaskNotFound(t, newStore(t)) })
	t.Run("TaskRequiresValidWorkspace", func(t *testing.T) { testTaskRequiresValidWorkspace(t, newStore(t)) })
	t.Run("TaskListByWorkspace", func(t *testing.T) { testTaskListByWorkspace(t, newStore(t)) })
	t.Run("DeleteWorkspaceWithTasksRejected", func(t *testing.T) { testDeleteWorkspaceWithTasksRejected(t, newStore(t)) })
	t.Run("DeleteTargetWithWorkspacesRejected", func(t *testing.T) { testDeleteTargetWithWorkspacesRejected(t, newStore(t)) })

	t.Run("Credential", func(t *testing.T) { testCredentialCRUD(t, newStore(t)) })
	t.Run("CredentialNotFound", func(t *testing.T) { testCredentialNotFound(t, newStore(t)) })
	t.Run("CredentialWorkspaceScoped", func(t *testing.T) { testCredentialWorkspaceScoped(t, newStore(t)) })
	t.Run("CredentialRequiresValidWorkspace", func(t *testing.T) { testCredentialRequiresValidWorkspace(t, newStore(t)) })
	t.Run("CredentialListIncludesGlobalAndScoped", func(t *testing.T) { testCredentialListIncludesGlobalAndScoped(t, newStore(t)) })
	t.Run("DeleteWorkspaceWithCredentialRejected", func(t *testing.T) { testDeleteWorkspaceWithCredentialRejected(t, newStore(t)) })
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
		ID:   "11111111-1111-1111-1111-111111111111",
		Name: "local",
		Kind: registry.TargetKindLocal,
	}
	if err := s.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	got, err := s.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
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
	if got.StartedAt != nil || got.CompletedAt != nil {
		t.Fatalf("GetTask = %+v, want nil StartedAt/CompletedAt on creation", got)
	}

	got.Status = registry.TaskStatusCompleted
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
