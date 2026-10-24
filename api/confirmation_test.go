package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// LOOM-123: a conversation lists its offers for their cards, and an offer
// past its deadline reads as expired even before anything settles it.
// An Approve or Deny dispatch carries the offer it answers.
func TestConversation_Confirmations(t *testing.T) {
	srv, dispatcher, store := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "ok", nil }
	token, _ := login(t, srv.URL, testPassword)
	ctx := context.Background()

	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token,
		map[string]string{"conversation_id": "c1", "message": "yes", "confirmation_id": "conf-live"}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dispatch = %d", resp.StatusCode)
	}
	// The dispatch says which offer it answered, so a client can retry a
	// failed answer with the same id.
	if got := pollDispatch(t, srv.URL, token, out.DispatchID); got.ConfirmationID != "conf-live" {
		t.Fatalf("GET dispatch confirmation_id = %q, want conf-live", got.ConfirmationID)
	}
	if d, err := store.GetDispatch(ctx, out.DispatchID); err != nil || d.ConfirmationID != "conf-live" {
		t.Fatalf("stored dispatch = %+v, %v; want confirmation conf-live", d, err)
	}

	now := time.Now().UTC()
	for _, c := range []*registry.Confirmation{
		{ID: "conf-live", ConversationID: "c1", DispatchID: out.DispatchID, Kind: registry.ConfirmationRunCommand,
			TargetName: "jet01", Command: "df -h", ExpiresAt: now.Add(10 * time.Minute)},
		{ID: "conf-old", ConversationID: "c1", Kind: registry.ConfirmationCloneRemote, Workspace: "my-app",
			GitRemote: "https://example.com/r.git", ExpiresAt: now.Add(-time.Minute)},
	} {
		if err := store.CreateConfirmation(ctx, c); err != nil {
			t.Fatalf("CreateConfirmation: %v", err)
		}
	}

	r := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/c1", token, nil)
	defer r.Body.Close()
	var conv struct {
		Confirmations []map[string]any `json:"confirmations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&conv); err != nil {
		t.Fatal(err)
	}
	if len(conv.Confirmations) != 2 {
		t.Fatalf("confirmations = %v", conv.Confirmations)
	}
	live, old := conv.Confirmations[0], conv.Confirmations[1]
	if live["id"] != "conf-live" || live["status"] != "pending" || live["command"] != "df -h" ||
		live["target_name"] != "jet01" || live["dispatch_id"] != out.DispatchID || live["kind"] != "run_command" {
		t.Errorf("live = %v", live)
	}
	if old["status"] != "expired" || old["git_remote"] != "https://example.com/r.git" {
		t.Errorf("old = %v, want expired", old)
	}
	// The workspace's name is workspace_name, as the target's is
	// target_name (freeze review item 10).
	if _, ok := old["workspace"]; ok || old["workspace_name"] != "my-app" {
		t.Errorf("old = %v, want workspace_name my-app and no workspace", old)
	}
	if _, ok := live["status_reason"]; ok {
		t.Errorf("live = %v, want no status_reason", live)
	}

	// LOOM-216: a yes refused afterwards is denied, and says why.
	if err := store.CreateConfirmation(ctx, &registry.Confirmation{ID: "conf-refused", ConversationID: "c1",
		Kind: registry.ConfirmationCreateTarget, TargetName: "builds", ExpiresAt: now.Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveConfirmation(ctx, "conf-refused", registry.ConfirmationDenied, "the plugin's machine limit is reached."); err != nil {
		t.Fatal(err)
	}
	r2 := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/c1", token, nil)
	defer r2.Body.Close()
	conv.Confirmations = nil
	if err := json.NewDecoder(r2.Body).Decode(&conv); err != nil {
		t.Fatal(err)
	}
	var refused map[string]any
	for _, c := range conv.Confirmations {
		if c["id"] == "conf-refused" {
			refused = c
		}
	}
	if refused["status"] != "denied" || refused["status_reason"] != "the plugin's machine limit is reached." {
		t.Errorf("refused = %v", refused)
	}
}
