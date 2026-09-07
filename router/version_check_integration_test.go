package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// realVersionCheckFixture builds the same real-infrastructure rig as
// TestIntegration_RealDispatch (real store, real local target/workspace,
// real LocalExecutor), but with a VersionCheck attached to the one
// registered agent type — command "claude --version" runs against the
// real claude binary installed in this environment (confirmed present
// while designing this feature; see router/README.md) via a real
// targets.LocalExecutor.RunOnce, not a fake. The actual launch command
// stays a plain echo, deliberately isolating "does the version-check
// primitive really shell out and parse real output" from "does launching
// claude interactively work" (a different, already-covered concern).
func realVersionCheckFixture(t *testing.T, vc router.VersionCheck) (*router.Router, *registry.Workspace, registry.Store) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()

	target := &registry.Target{ID: uuid.NewString(), Name: "real-vc-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID: uuid.NewString(), Name: "real-vc-ws-" + t.Name(), Path: t.TempDir(),
		TargetID: target.ID, Status: registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	agentTypes := router.AgentTypeRegistry{
		"echo-agent": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 300 * time.Millisecond},
			LaunchTemplate: `sh -c 'echo integration-test-output; sleep 30'`,
			VersionCheck:   &vc,
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "echo-agent"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "relayed: " + strings.TrimSpace(captured), Done: true}, nil
		},
	}

	r := router.New(store, orch, targets.NewExecutor, resolver, agentTypes, model, markerDir)
	return r, ws, store
}

func TestIntegration_RealVersionCheck_SatisfiedBound_LaunchesNormally(t *testing.T) {
	r, _, _ := realVersionCheckFixture(t, router.VersionCheck{
		Command: "claude --version",
		Parse:   router.ExtractDottedVersion,
		Min:     "0.0.1", // trivially satisfied by whatever real version is installed
	})

	reply, err := r.Dispatch(context.Background(), "conv-real-vc", "do the real thing")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "integration-test-output") {
		t.Fatalf("reply = %q, want it to contain the real pane's output — the real version check must have passed for the launch to reach this point", reply)
	}
}

func TestIntegration_RealVersionCheck_UnsatisfiedBound_FailsLoudBeforeLaunch(t *testing.T) {
	r, ws, store := realVersionCheckFixture(t, router.VersionCheck{
		Command: "claude --version",
		Parse:   router.ExtractDottedVersion,
		Min:     "999.0.0", // no real installed version will ever satisfy this
	})

	_, err := r.Dispatch(context.Background(), "conv-real-vc-fail", "do the real thing")
	if err == nil {
		t.Fatal("Dispatch against a real claude binary below an impossible minimum: want error, got nil")
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("ListTasksByWorkspace = %+v, want 0 — real launch must fail loud before any task record is created", tasks)
	}
}
