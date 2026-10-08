package router_test

// LOOM-139 audit regressions.

import (
	"context"
	"testing"

	"github.com/Loomux/server/router"
)

// A model-chosen remote skips the clone confirmation only when the user
// named that exact repository: a URL that merely contains it as a prefix
// is not the user asking for that clone.
func TestAudit_Provision_CloneConfirmationNotSkippedBySubstring(t *testing.T) {
	const msg = "set up https://github.com/alice/tools-fork and look around"
	store, exec, r, model := setup(t)
	target := createFixtureTarget(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "claude-code", NewWorkspace: router.ProvisionSpec{
			Name: "tools", TargetID: target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://github.com/alice/tools",
		}}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "relayed", Done: true}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-sub", msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := exec.launchedCommands(); len(cmds) != 0 {
		t.Errorf("cloned without confirmation: %v", cmds)
	}
}
