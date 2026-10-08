package router_test

// LOOM-139 audit regressions. A test whose bug is still open skips naming
// its finding; LOOMUX_AUDIT_XFAIL=1 runs it.

import (
	"context"
	"os"
	"testing"

	"github.com/Loomux/server/router"
)

func skipUntilFixed(t *testing.T, issue string) {
	t.Helper()
	if os.Getenv("LOOMUX_AUDIT_XFAIL") == "" {
		t.Skipf("known bug, see %s (LOOMUX_AUDIT_XFAIL=1 runs it)", issue)
	}
}

// A model-chosen remote skips the clone confirmation only when the user
// named that exact repository: a URL that merely contains it as a prefix
// is not the user asking for that clone.
func TestAudit_Provision_CloneConfirmationNotSkippedBySubstring(t *testing.T) {
	skipUntilFixed(t, "LOOM-139 finding F13")
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
