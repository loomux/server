package router_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// unreadableVault is a store whose vault can be made unreadable.
type unreadableVault struct {
	registry.Store
	fail atomic.Bool
}

func (s *unreadableVault) ListCredentials(ctx context.Context) ([]*registry.Credential, error) {
	if s.fail.Load() {
		return nil, errors.New("vault unreadable")
	}
	return s.Store.ListCredentials(ctx)
}

// The message the routing model routes is redacted against the vault;
// the agent it goes to gets it as the user typed it.
func TestDispatch_MessagePassedToDecideRedactsVaultValues(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"claude-code": true})
	ctx := context.Background()
	const secret = "sk-PROBE-current-message-value-123"
	if err := h.store.CreateCredential(ctx, &registry.Credential{
		ID: uuid.NewString(), Name: "MY_TOKEN", Value: secret,
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	h.decide(h.provisionDecision("claude-code"))
	if _, err := h.r.Dispatch(ctx, "conv-1", "Deploy with the token "+secret); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := h.model.LastDecideMessage; got != "Deploy with the token [redacted]" {
		t.Errorf("the routing model got %q, want the vault value redacted", got)
	}
	if sent := h.sentToAgent(t, "claude-code"); !strings.Contains(sent, "Deploy with the token "+secret) {
		t.Errorf("the agent got %q, want the message as typed", sent)
	}
}

// If the vault can't be read, the message isn't routed: the turn fails
// and the routing model is never called.
func TestDispatch_VaultUnreadableMessageNotRouted(t *testing.T) {
	store := &unreadableVault{Store: newTestStore(t)}
	_, r, model := newRouter(t, store)
	decided := false
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		decided = true
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	store.fail.Store(true)

	if _, err := r.Dispatch(context.Background(), "conv-1", "hello"); err == nil {
		t.Error("Dispatch: want an error when the vault can't be read")
	}
	if decided {
		t.Error("the routing model was called, want the message withheld from it")
	}
}

// If the vault can't be read, the routing model gets no history and a
// fresh agent no earlier-conversation note.
func TestConversation_VaultUnreadableNoHistoryNoNote(t *testing.T) {
	store := &unreadableVault{Store: newTestStore(t)}
	_, r, model := newRouter(t, store)
	ws := createFixtureWorkspace(t, store)
	ctx := context.Background()
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Which workspace?"}, nil
	}
	if _, err := r.Dispatch(ctx, "conv-1", "Set up the deploy"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// With the vault readable, both carry the turn.
	history, err := router.ConversationHistory(ctx, r, "conv-1")
	if err != nil || len(history) == 0 {
		t.Fatalf("history = %+v, %v; want the earlier turn", history, err)
	}
	note, err := router.EarlierConversation(ctx, r, "conv-1", ws.ID, "the fixture one")
	if err != nil || !strings.Contains(note, "Set up the deploy") {
		t.Fatalf("note = %q, %v; want the earlier turn", note, err)
	}

	store.fail.Store(true)
	history, err = router.ConversationHistory(ctx, r, "conv-1")
	if err != nil || len(history) != 0 {
		t.Errorf("history = %+v, %v; want none when the vault can't be read", history, err)
	}
	note, err = router.EarlierConversation(ctx, r, "conv-1", ws.ID, "the fixture one")
	if err != nil || note != "" {
		t.Errorf("note = %q, %v; want none when the vault can't be read", note, err)
	}
}
