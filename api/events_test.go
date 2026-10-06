package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-110: a conversation's audit trail, oldest first, behind auth.
func TestConversationEvents(t *testing.T) {
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option { return []api.Option{api.WithEvents(s)} })
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Minute)
	for _, e := range []*registry.DispatchEvent{
		{ID: "e1", ConversationID: "c1", DispatchID: "d1", CreatedAt: t0, Kind: registry.EventDecision,
			Model: "m", Tier: "primary", TargetID: "t1", Command: "df -h", Outcome: "run_command", Duration: 1500 * time.Millisecond},
		{ID: "e2", ConversationID: "c1", DispatchID: "d1", CreatedAt: t0.Add(time.Second), Kind: registry.EventOutcome,
			Outcome: "failure", ErrorClass: registry.ErrorClassTimeout},
		{ID: "e3", ConversationID: "c2", Kind: registry.EventDecision},
	} {
		if err := store.CreateDispatchEvent(ctx, e); err != nil {
			t.Fatalf("CreateDispatchEvent: %v", err)
		}
	}

	if resp, err := http.Get(srv.URL + "/api/v1/conversations/c1/events"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %v, %v; want 401", resp, err)
	}
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/c1/events", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET events = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()
	var body struct {
		ConversationID string           `json:"conversation_id"`
		Events         []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ConversationID != "c1" || len(body.Events) != 2 {
		t.Fatalf("body = %+v, want c1's two events", body)
	}
	first, second := body.Events[0], body.Events[1]
	if first["kind"] != "decision" || first["model"] != "m" || first["tier"] != "primary" || first["command"] != "df -h" ||
		first["duration_ms"] != float64(1500) || first["dispatch_id"] != "d1" {
		t.Errorf("first event = %v", first)
	}
	if second["kind"] != "outcome" || second["error_class"] != "timeout" {
		t.Errorf("second event = %v", second)
	}
	if _, ok := second["model"]; ok {
		t.Errorf("second event = %v, want empty fields left out", second)
	}
}
