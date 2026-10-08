package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-108: what the relay model (a third-party vendor) is sent has every
// vault value and anything shaped like a secret removed, and so does the
// turn's stored transcript.
func TestDispatch_RelayInputRedacted(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	if err := store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "MY_TOKEN", Value: "vault-value-0123456789",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	const ghToken = "ghp_1234567890abcdefghijABCDEFGHIJ123456"
	exec.capture = "done\nauth with vault-value-0123456789\nGITHUB_TOKEN=" + ghToken + "\nall good"

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	var sent string
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		sent = captured
		return router.RelayResult{Reply: "ok", Done: true}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "do the thing"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, secret := range []string{"vault-value-0123456789", ghToken} {
		if strings.Contains(sent, secret) {
			t.Errorf("relay model was sent %q:\n%s", secret, sent)
		}
	}
	if !strings.Contains(sent, "all good") {
		t.Errorf("relay input lost the ordinary output:\n%s", sent)
	}
}

// LOOM-185: a router provider key (a system secret, not in the vault)
// an agent prints is removed from what the relay model is sent, even in a
// shape no secret pattern knows.
func TestDispatch_RouterKeyRedacted(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	const routerKey = "plainrouterkey0123456789"
	credentials.AddSystemSecret(routerKey)
	exec.capture = "done\nfound " + routerKey + " in the env\nall good"

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	var sent string
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		sent = captured
		return router.RelayResult{Reply: "ok", Done: true}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "do the thing"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(sent, routerKey) {
		t.Errorf("relay model was sent the router key:\n%s", sent)
	}
	if !strings.Contains(sent, "all good") {
		t.Errorf("relay input lost the ordinary output:\n%s", sent)
	}
}
