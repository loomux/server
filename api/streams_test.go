package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
)

// watchEnd reads the stream r to its end in the background; the
// returned channel closes when it ends.
func watchEnd(r interface{ ReadString(byte) (string, error) }) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	return done
}

// endsWithin reports whether ended closes within d.
func endsWithin(ended <-chan struct{}, d time.Duration) bool {
	select {
	case <-ended:
		return true
	case <-time.After(d):
		return false
	}
}

// LOOM-144: revoking a session from another device ends its open
// streams at once; other sessions' streams carry on.
func TestStream_EndsWhenSessionRevokedElsewhere(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	victim, _ := login(t, srv.URL, testPassword)
	other, _ := login(t, srv.URL, testPassword)
	victimEnded := watchEnd(openStream(t, srv.URL, victim, "c-rev"))
	otherEnded := watchEnd(openStream(t, srv.URL, other, "c-rev"))
	time.Sleep(50 * time.Millisecond)

	_, sessions := listSessions(t, srv.URL, other)
	var victimID string
	for _, s := range sessions {
		if !s.Current {
			victimID = s.ID
		}
	}
	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/"+victimID, other, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if !endsWithin(victimEnded, 5*time.Second) {
		t.Fatal("the revoked session's stream stayed open")
	}
	if endsWithin(otherEnded, 300*time.Millisecond) {
		t.Fatal("revoking one session ended another's stream")
	}
}

// LOOM-144: a stream whose session expires while it's open ends at the
// next heartbeat's session check.
func TestStream_EndsWhenSessionExpires(t *testing.T) {
	srv, _, store := newTestServer(t,
		api.WithStreamPollInterval(10*time.Millisecond),
		api.WithStreamHeartbeat(50*time.Millisecond),
		api.WithSessionTTL(time.Hour))
	token, _ := login(t, srv.URL, testPassword)
	ended := watchEnd(openStream(t, srv.URL, token, "c-exp"))
	time.Sleep(100 * time.Millisecond)
	if endsWithin(ended, 200*time.Millisecond) {
		t.Fatal("a live session's stream ended")
	}

	sessions, err := store.ListSessions(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions: %v, %d", err, len(sessions))
	}
	if err := store.TouchSession(context.Background(), sessions[0].ID, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !endsWithin(ended, 5*time.Second) {
		t.Fatal("the expired session's stream stayed open")
	}
}

// LOOM-147: EndStreams ends every open stream, so a server shutdown
// needn't wait out its grace period for them.
func TestStream_EndStreamsEndsThemAll(t *testing.T) {
	store := newTestStore(t)
	jobs := dispatch.New(store, func(context.Context, *registry.Dispatch) (string, error) { return "", nil })
	server := api.NewServer(jobs, store, store, store, store, store, store, []byte(testPasswordHash(t)))
	hs := httptest.NewServer(server)
	t.Cleanup(hs.Close)
	a, _ := login(t, hs.URL, testPassword)
	b, _ := login(t, hs.URL, testPassword)
	endedA := watchEnd(openStream(t, hs.URL, a, "c-1"))
	endedB := watchEnd(openStream(t, hs.URL, b, "c-2"))
	time.Sleep(50 * time.Millisecond)
	server.EndStreams()
	if !endsWithin(endedA, 5*time.Second) || !endsWithin(endedB, 5*time.Second) {
		t.Fatal("a stream stayed open after EndStreams")
	}
}

// A stream that connects after EndStreams (it was still being
// authenticated as shutdown began) ends at once too.
func TestStream_ConnectingAfterEndStreamsEndsAtOnce(t *testing.T) {
	store := newTestStore(t)
	jobs := dispatch.New(store, func(context.Context, *registry.Dispatch) (string, error) { return "", nil })
	server := api.NewServer(jobs, store, store, store, store, store, store, []byte(testPasswordHash(t)))
	hs := httptest.NewServer(server)
	t.Cleanup(hs.Close)
	token, _ := login(t, hs.URL, testPassword)
	server.EndStreams()
	if !endsWithin(watchEnd(openStream(t, hs.URL, token, "c-late")), 5*time.Second) {
		t.Fatal("a stream opened after EndStreams stayed open")
	}
}
