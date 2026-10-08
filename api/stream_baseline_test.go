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
// what existed at connect, not at the first tick.
func TestStream_QuickTurnAfterConnectIsReported(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithStreamPollInterval(streamGap))
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "quick", nil }
	token, _ := login(t, srv.URL, testPassword)

	r := openStream(t, srv.URL, token, "c-quick")
	start := time.Now()
	_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token,
		map[string]string{"conversation_id": "c-quick", "message": "hi"}, nil)
	if d.Status != "succeeded" {
		t.Fatalf("dispatch = %+v, want it finished", d)
	}
	if time.Since(start) >= streamGap {
		t.Skipf("the turn took %s, longer than the first poll interval: it doesn't exercise the race", time.Since(start))
	}
	u := readDispatchUpdate(t, r)
	if u.DispatchID != d.DispatchID || u.Status != "succeeded" || u.Reply != "quick" {
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
