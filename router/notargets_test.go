package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

// TestDispatch_NoTargets_TargetActionAnswersRegisterTarget (LOOM-68):
// with zero registered targets, a routing decision to provision a
// workspace or run a command — which no machine exists for — gets a
// clear direct answer telling the user to register a target, not an
// error; nothing is launched and no workspace row is written.
func TestDispatch_NoTargets_TargetActionAnswersRegisterTarget(t *testing.T) {
	for _, dec := range []router.Decision{
		{Action: router.ActionProvisionWorkspace, AgentType: "claude-code",
			NewWorkspace: router.ProvisionSpec{Name: "x", TargetID: "sc1", Kind: router.ProvisionEmpty}},
		{Action: router.ActionRunCommand, TargetID: "sc1", Command: "uptime"},
	} {
		store, exec, r, model := setup(t)
		model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return dec, nil
		}

		reply, err := r.Dispatch(context.Background(), "conv-1", "do the thing")
		if err != nil {
			t.Fatalf("%s: Dispatch: %v", dec.Action, err)
		}
		if reply != router.NoTargetsReply {
			t.Errorf("%s: reply = %q, want router.NoTargetsReply", dec.Action, reply)
		}
		if len(model.LastDecideTargets) != 0 {
			t.Errorf("%s: model was told about targets %+v, want none", dec.Action, model.LastDecideTargets)
		}
		if len(exec.sessions) != 0 {
			t.Errorf("%s: launched sessions %+v, want none", dec.Action, exec.sessions)
		}
		ws, err := store.ListWorkspaces(context.Background())
		if err != nil {
			t.Fatalf("ListWorkspaces: %v", err)
		}
		if len(ws) != 0 {
			t.Errorf("%s: workspaces written %+v, want none", dec.Action, ws)
		}
	}
}

// The reply names both ways to register a target (LOOM-68).
func TestNoTargetsReply_NamesBothWaysToRegister(t *testing.T) {
	for _, want := range []string{"POST /api/v1/targets", "Targets page"} {
		if !strings.Contains(router.NoTargetsReply, want) {
			t.Errorf("NoTargetsReply = %q, missing %q", router.NoTargetsReply, want)
		}
	}
}
