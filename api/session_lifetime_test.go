package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
)

// LOOM-175: a session ends at its absolute lifetime however recently it
// was used: requests keep its sliding window fresh, and it still expires,
// for requests, the session list and an open stream alike. With no
// absolute lifetime it lives on.
func TestSession_AbsoluteLifetime(t *testing.T) {
	const maxAge = 400 * time.Millisecond
	srv, _, _ := newTestServer(t, api.WithSessionMaxAge(maxAge),
		api.WithStreamPollInterval(10*time.Millisecond), api.WithStreamHeartbeat(50*time.Millisecond))
	token, _ := login(t, srv.URL, testPassword)
	ended := watchEnd(openStream(t, srv.URL, token, "c-abs"))

	deadline := time.Now().Add(maxAge + 300*time.Millisecond)
	for time.Now().Before(deadline) {
		resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/sessions", token, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/sessions", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session in use past its absolute lifetime: %d, want 401", resp.StatusCode)
	}
	if !endsWithin(ended, 5*time.Second) {
		t.Error("its stream stayed open")
	}

	// Another session's list doesn't show a session past it either.
	other, _ := login(t, srv.URL, testPassword)
	if _, sessions := listSessions(t, srv.URL, other); len(sessions) != 1 {
		t.Errorf("sessions = %+v, want only the new one", sessions)
	}

	off, _, _ := newTestServer(t, api.WithSessionMaxAge(0))
	offToken, _ := login(t, off.URL, testPassword)
	time.Sleep(maxAge)
	resp = authedRequest(t, http.MethodGet, off.URL+"/api/v1/sessions", offToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("with no absolute lifetime: %d, want 200", resp.StatusCode)
	}
}
