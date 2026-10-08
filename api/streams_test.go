package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// postDispatchAsync starts a blocking POST /api/v1/dispatch?wait=true in
// the background; the returned channel delivers its status and body.
func postDispatchAsync(t *testing.T, baseURL, token string) <-chan dispatchResult {
	t.Helper()
	out := make(chan dispatchResult, 1)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/dispatch?wait=true",
		strings.NewReader(`{"conversation_id":"c-wait","message":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- dispatchResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		out <- dispatchResult{status: resp.StatusCode, body: string(body), err: err}
	}()
	return out
}

type dispatchResult struct {
	status int
	body   string
	err    error
}

// LOOM-182: a blocking dispatch started before logout doesn't hand its
// reply to the revoked token: the wait ends with the session, 401, and
// the turn itself carries on.
func TestBlockingDispatch_EndsWithSession(t *testing.T) {
	srv, fd, store := newTestServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	fd.DispatchFunc = func(ctx context.Context, _, _, _ string) (string, error) {
		close(started)
		<-release
		return "secret reply", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	result := postDispatchAsync(t, srv.URL, token)
	<-started

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/logout", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	var got dispatchResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		close(release)
		got = <-result
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.status != http.StatusUnauthorized || strings.Contains(got.body, "secret reply") {
		t.Fatalf("dispatch after logout = %d %s, want 401 without the reply", got.status, got.body)
	}

	// The turn wasn't cancelled with the wait: it still finishes, and its
	// result is recorded.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ds, err := store.ListDispatchesByConversation(context.Background(), "c-wait")
		if err == nil && len(ds) == 1 && ds[0].Reply == "secret reply" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the turn's result was never recorded: %+v, %v", ds, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LOOM-182: revoking another session leaves a blocking dispatch's wait
// alone.
func TestBlockingDispatch_OtherSessionsRevokeDoesNotEndIt(t *testing.T) {
	srv, fd, _ := newTestServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	fd.DispatchFunc = func(ctx context.Context, _, _, _ string) (string, error) {
		close(started)
		<-release
		return "the reply", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	other, _ := login(t, srv.URL, testPassword)
	result := postDispatchAsync(t, srv.URL, token)
	<-started

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/logout", other, nil)
	resp.Body.Close()
	close(release)
	got := <-result
	if got.err != nil || got.status != http.StatusOK || !strings.Contains(got.body, "the reply") {
		t.Fatalf("dispatch = %d %s %v, want 200 with the reply", got.status, got.body, got.err)
	}
}

// LOOM-182 with LOOM-147: shutdown's EndStreams ends streams, not
// blocking dispatch waits; those are left to the HTTP drain as before,
// and get their reply.
func TestBlockingDispatch_NotEndedByEndStreams(t *testing.T) {
	store := newTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	jobs := dispatch.New(store, func(context.Context, *registry.Dispatch) (string, error) {
		close(started)
		<-release
		return "the reply", nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jobs.Shutdown(ctx)
	})
	server := api.NewServer(jobs, store, store, store, store, store, store, []byte(testPasswordHash(t)))
	hs := httptest.NewServer(server)
	t.Cleanup(hs.Close)
	token, _ := login(t, hs.URL, testPassword)
	result := postDispatchAsync(t, hs.URL, token)
	<-started

	server.EndStreams()
	close(release)
	got := <-result
	if got.err != nil || got.status != http.StatusOK || !strings.Contains(got.body, "the reply") {
		t.Fatalf("dispatch after EndStreams = %d %s %v, want 200 with the reply", got.status, got.body, got.err)
	}
}
