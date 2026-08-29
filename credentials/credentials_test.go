package credentials_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

func newTestStore(t *testing.T) registry.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath, sqlite.WithMasterKey([]byte("01234567890123456789012345678901"[:32])))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func createFixtureWorkspace(t *testing.T, store registry.Store) *registry.Workspace {
	t.Helper()
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "fixture-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "fixture-ws-" + t.Name(), Path: "/fixture", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}

func createCredential(t *testing.T, store registry.Store, name, workspaceID, agentType, value string) {
	t.Helper()
	c := &registry.Credential{ID: uuid.NewString(), Name: name, WorkspaceID: workspaceID, AgentType: agentType, Value: value}
	if err := store.CreateCredential(context.Background(), c); err != nil {
		t.Fatalf("CreateCredential(%s): %v", name, err)
	}
}

func TestResolve_GlobalOnly(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "GLOBAL_TOKEN", "", "", "global-value")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["GLOBAL_TOKEN"] != "global-value" {
		t.Fatalf("Resolve()[GLOBAL_TOKEN] = %q, want %q", got["GLOBAL_TOKEN"], "global-value")
	}
}

func TestResolve_WorkspaceOverridesGlobal(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "TOKEN", "", "", "global-value")
	createCredential(t, store, "TOKEN", ws.ID, "", "workspace-value")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN"] != "workspace-value" {
		t.Fatalf("Resolve()[TOKEN] = %q, want %q (workspace overrides global)", got["TOKEN"], "workspace-value")
	}
}

func TestResolve_AgentTypeOverridesGlobal(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "TOKEN", "", "", "global-value")
	createCredential(t, store, "TOKEN", "", "claude-code", "agent-value")

	r := credentials.NewResolver(store)

	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN"] != "agent-value" {
		t.Fatalf("Resolve(claude-code)[TOKEN] = %q, want %q", got["TOKEN"], "agent-value")
	}

	got, err = r.Resolve(context.Background(), ws.ID, "codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN"] != "global-value" {
		t.Fatalf("Resolve(codex)[TOKEN] = %q, want %q (agent-type-scoped entry for a different agent shouldn't apply)", got["TOKEN"], "global-value")
	}
}

func TestResolve_MostSpecificWins(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "TOKEN", "", "", "global")
	createCredential(t, store, "TOKEN", ws.ID, "", "workspace-only")
	createCredential(t, store, "TOKEN", "", "claude-code", "agent-only")
	createCredential(t, store, "TOKEN", ws.ID, "claude-code", "workspace-and-agent")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN"] != "workspace-and-agent" {
		t.Fatalf("Resolve()[TOKEN] = %q, want %q (most specific: workspace+agentType)", got["TOKEN"], "workspace-and-agent")
	}
}

func TestResolve_WorkspaceBeatsAgentTypeAtEqualSpecificity(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "TOKEN", ws.ID, "", "workspace-only")
	createCredential(t, store, "TOKEN", "", "claude-code", "agent-only")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN"] != "workspace-only" {
		t.Fatalf("Resolve()[TOKEN] = %q, want %q (workspace-scoped beats agent-type-scoped at equal specificity, by documented tie-break)", got["TOKEN"], "workspace-only")
	}
}

func TestResolve_ExcludesOtherWorkspaceAndOtherAgentType(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	otherWs := createFixtureWorkspace2(t, store)

	createCredential(t, store, "OTHER_WS_TOKEN", otherWs.ID, "", "should-not-appear")
	createCredential(t, store, "OTHER_AGENT_TOKEN", "", "some-other-agent", "should-not-appear")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := got["OTHER_WS_TOKEN"]; ok {
		t.Fatalf("Resolve() included a credential scoped to a different workspace: %v", got)
	}
	if _, ok := got["OTHER_AGENT_TOKEN"]; ok {
		t.Fatalf("Resolve() included a credential scoped to a different agent type: %v", got)
	}
}

func TestResolve_MultipleNamesAllIncluded(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	createCredential(t, store, "TOKEN_A", "", "", "value-a")
	createCredential(t, store, "TOKEN_B", "", "", "value-b")

	r := credentials.NewResolver(store)
	got, err := r.Resolve(context.Background(), ws.ID, "claude-code")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["TOKEN_A"] != "value-a" || got["TOKEN_B"] != "value-b" {
		t.Fatalf("Resolve() = %v, want both TOKEN_A and TOKEN_B", got)
	}
}

// createFixtureWorkspace2 builds a second, distinctly-named fixture
// workspace, for tests that need two workspaces to prove one's
// credentials don't leak into the other's resolution.
func createFixtureWorkspace2(t *testing.T, store registry.Store) *registry.Workspace {
	t.Helper()
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "fixture-target-2-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "fixture-ws-2-" + t.Name(), Path: "/fixture2", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}
