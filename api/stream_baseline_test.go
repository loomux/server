package api_test

import (
	"context"
	"encoding/json"

	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// streamGap is a poll interval long enough that a quick turn can start
// and finish between the stream's connect and its first poll tick.
const streamGap = 400 * time.Millisecond

// LOOM-137: a turn that starts and finishes before the stream's first
// poll tick is still reported, with its terminal update: the baseline is
// what existed at connect, not at the first tick. The job is written
// straight to the store, already finished, so the test doesn't depend on
// how long a turn takes.
func TestStream_QuickTurnAfterConnectIsReported(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(streamGap))
	token, _ := login(t, srv.URL, testPassword)

	r := openStream(t, srv.URL, token, "c-quick")
	now := time.Now().UTC()
	if err := store.CreateDispatch(context.Background(), &registry.Dispatch{ID: "d-quick", ConversationID: "c-quick",
		Message: "hi", Status: registry.DispatchStatusSucceeded, Reply: "quick", CreatedAt: now, StartedAt: &now,
		FinishedAt: &now}, nil); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	u := readDispatchUpdate(t, r)
	if u.DispatchID != "d-quick" || u.Status != "succeeded" || u.Reply != "quick" {
		t.Fatalf("dispatch_update = %+v, want the quick turn's terminal update", u)
	}
}

// The same for a message logged in that first interval (LOOM-121's
// message_added).
func TestStream_MessageRightAfterConnectIsReported(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(streamGap))
	token, _ := login(t, srv.URL, testPassword)
	r := openStream(t, srv.URL, token, "c-quick-msg")
	if err := store.CreateMessage(context.Background(), &registry.Message{ID: "m-quick", ConversationID: "c-quick-msg",
		Role: registry.MessageRoleAssistant, Content: "late"}); err != nil {
		t.Fatal(err)
	}
	for {
		ev := readSSEEventWithTimeout(t, r, 3*time.Second)
		if ev.Event != "message_added" {
			continue
		}
		var m struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &m); err != nil || m.MessageID != "m-quick" {
			t.Fatalf("message_added %s, want m-quick", ev.Data)
		}
		return
	}
}
