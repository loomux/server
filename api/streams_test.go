package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
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
