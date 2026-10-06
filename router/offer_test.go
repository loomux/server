package router_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-107: with more workspaces than MaxOfferedWorkspaces, the routing
// model sees the most recent ones plus the hinted one, however old.
func TestDispatch_CapsOfferedWorkspaces(t *testing.T) {
	store, _, r, model := setup(t)
	target := createFixtureTarget(t, store)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	var oldest string
	for i := 0; i < router.MaxOfferedWorkspaces+5; i++ {
		used := base.Add(time.Duration(i) * time.Minute)
		ws := &registry.Workspace{ID: uuid.NewString(), Name: fmt.Sprintf("ws-%02d", i), TargetID: target.ID, Status: registry.WorkspaceStatusIdle, LastUsedAt: &used}
		if err := store.CreateWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = ws.ID
		}
	}
	var seen []router.WorkspaceSnapshot
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		seen = workspaces
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(ctx, "conv-1", "hello", router.WithWorkspaceHint(oldest)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(seen) != router.MaxOfferedWorkspaces {
		t.Fatalf("model saw %d workspaces, want %d", len(seen), router.MaxOfferedWorkspaces)
	}
	found := false
	for _, ws := range seen {
		found = found || ws.ID == oldest
	}
	if !found {
		t.Error("the hinted workspace was cut")
	}
	if seen[0].Name != fmt.Sprintf("ws-%02d", router.MaxOfferedWorkspaces+4) {
		t.Errorf("first offered = %s, want the most recently used", seen[0].Name)
	}
}
