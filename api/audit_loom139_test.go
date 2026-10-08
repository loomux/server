package api_test

// LOOM-139 audit regressions. Each test pins a behaviour the audit found
// wrong. A test whose bug is still open skips naming its finding, so CI
// stays green while the bug is real; LOOMUX_AUDIT_XFAIL=1 runs it, and the
// fix removes the skip.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// skipUntilFixed skips a test that pins an open audit bug.
func skipUntilFixed(t *testing.T, issue string) {
	t.Helper()
	if os.Getenv("LOOMUX_AUDIT_XFAIL") == "" {
		t.Skipf("known bug, see %s (LOOMUX_AUDIT_XFAIL=1 runs it)", issue)
	}
}

// Concurrent wrong passwords all pass throttle.wait() before any of them
// records its failure, so a burst gets one guess each instead of one per
// backoff window.
func TestAudit_LoginThrottle_ConcurrentGuessesShareOneWindow(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(time.Hour, time.Hour))

	const n = 20
	var start sync.WaitGroup
	start.Add(1)
	statuses := make(chan int, n)
	var done sync.WaitGroup
	for i := 0; i < n; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			resp, err := http.Post(srv.URL+"/api/v1/login", "application/json", strings.NewReader(`{"password":"wrong"}`))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	start.Done()
	done.Wait()
	close(statuses)

	checked := 0
	for s := range statuses {
		if s == http.StatusUnauthorized {
			checked++
		}
	}
	if checked != 1 {
		t.Fatalf("%d of %d concurrent wrong passwords were checked (401), want exactly 1; the rest should be 429", checked, n)
	}
}

// conversation_id is a path segment everywhere else in the API, so a
// client-supplied one must be a well-formed id, not any string.
func TestAudit_Dispatch_RejectsMalformedConversationID(t *testing.T) {
	skipUntilFixed(t, "LOOM-139 finding F15")
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "ok", nil }
	token, _ := login(t, srv.URL, testPassword)

	for _, id := range []string{"a/b", "../x", strings.Repeat("x", 10_000), " ", "<script>"} {
		resp, _ := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": id, "message": "hi"}, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("conversation_id %.20q: status %d, want 400", id, resp.StatusCode)
		}
	}
}

// A retry of a new-conversation dispatch with the same Idempotency-Key must
// return the original dispatch (design: "uuid per submit, reused on
// network retry"), not 422.
func TestAudit_Dispatch_IdempotentRetryOfNewConversation(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "once", nil }
	token, _ := login(t, srv.URL, testPassword)
	body := map[string]string{"message": "start something"}
	hdr := map[string]string{"Idempotency-Key": "retry-new-conv"}

	r1, a := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, body, hdr)
	r2, b := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, body, hdr)
	if r1.StatusCode != http.StatusOK || r2.StatusCode != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 200, 200", r1.StatusCode, r2.StatusCode)
	}
	if a.DispatchID != b.DispatchID || a.ConversationID != b.ConversationID {
		t.Fatalf("retry got %+v, want the original %+v", b, a)
	}
}

// A message the router will refuse anyway (over targets.MaxPasteBytes)
// must be refused before any routing call or provisioning.
func TestAudit_Dispatch_RejectsOversizedMessageUpFront(t *testing.T) {
	skipUntilFixed(t, "LOOM-139 finding F16")
	srv, dispatcher, _ := newTestServer(t)
	called := make(chan struct{}, 1)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		called <- struct{}{}
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	resp, _ := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token,
		map[string]string{"message": strings.Repeat("a", 200_000)}, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 413 or 400", resp.StatusCode)
	}
	select {
	case <-called:
		t.Error("the router was called with an oversized message")
	default:
	}
}

// Revoking a session (or logging out) must also end the event streams it
// opened; today requireAuth only runs at connect.
func TestAudit_Stream_StopsAfterSessionRevoked(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	ctx := context.Background()
	token, _ := login(t, srv.URL, testPassword)
	r := openStream(t, srv.URL, token, "c-revoke")
	time.Sleep(50 * time.Millisecond)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/logout", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if err := store.CreateMessage(ctx, &registry.Message{ID: "m-after", ConversationID: "c-revoke", Role: registry.MessageRoleAssistant, Content: "secret after logout"}); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)
	go func() {
		for {
			ev, err := readSSEEvent(t, r)
			if err != nil {
				got <- "closed"
				return
			}
			if ev.Event == "message_added" {
				got <- ev.Data
				return
			}
		}
	}()
	select {
	case v := <-got:
		if v != "closed" {
			t.Fatalf("a logged-out stream still delivered %s", v)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stream stayed open 20s after logout")
	}
}

func staticServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range map[string]string{"index.html": "<html>shell</html>", "assets/app-1.js": "x"} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, _, _ := newTestServer(t, api.WithStaticDir(dir))
	return srv.URL
}

// A missing hashed asset (e.g. a chunk from the bundle before a web
// update) must 404, not answer 200 with index.html: the browser then
// fails with a MIME error instead of a clean chunk-load retry.
func TestAudit_Static_MissingAssetIs404(t *testing.T) {
	skipUntilFixed(t, "LOOM-139 finding F19")
	base := staticServer(t)
	resp, err := http.Get(base + "/assets/app-old.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset: status %d (%s), want 404", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// The app shell holds the bearer token in localStorage; it needs the
// standard browser hardening headers.
func TestAudit_Static_SecurityHeaders(t *testing.T) {
	base := staticServer(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := resp.Header
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options: nosniff missing")
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors") || !strings.Contains(csp, "script-src") {
		t.Errorf("Content-Security-Policy = %q, want script-src and frame-ancestors", csp)
	}
	if h.Get("Referrer-Policy") == "" {
		t.Error("Referrer-Policy missing")
	}
}

// Unknown paths under /api/v1/ answer plain text today; every other API
// error is {error, code}.
func TestAudit_UnknownAPIPath_IsJSONError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("GET /api/v1/nonexistent = %d %q %q, want a 404 {error, code}", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func loginWith(t *testing.T, baseURL string, body map[string]string) (status int, token, device string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/v1/login", "application/json", bytes.NewReader(mustJSON(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Token, Device string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Token, out.Device
}

// A stranger failing at /login over and over must not keep the owner out
// of a browser that has logged in before (LOOM-151): its device token
// puts it on its own backoff.
func TestLogin_KnownDeviceNotLockedOutByStrangers(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(time.Hour, time.Hour))

	status, _, device := loginWith(t, srv.URL, map[string]string{"password": testPassword})
	if status != http.StatusOK || device == "" {
		t.Fatalf("first login = %d, device %q; want 200 and a device token", status, device)
	}
	if status, _, _ := loginWith(t, srv.URL, map[string]string{"password": "stranger-guess"}); status != http.StatusUnauthorized {
		t.Fatalf("stranger's guess = %d, want 401", status)
	}
	if status, _, _ := loginWith(t, srv.URL, map[string]string{"password": testPassword}); status != http.StatusTooManyRequests {
		t.Fatalf("login without a device after the stranger's failure = %d, want 429 (global backoff)", status)
	}
	if status, _, _ := loginWith(t, srv.URL, map[string]string{"password": testPassword, "device": "forged"}); status != http.StatusTooManyRequests {
		t.Fatalf("login with a forged device = %d, want 429 (global backoff)", status)
	}
	status, token, again := loginWith(t, srv.URL, map[string]string{"password": testPassword, "device": device})
	if status != http.StatusOK || token == "" {
		t.Fatalf("owner's login from a known device = %d, want 200", status)
	}
	if again != device {
		t.Errorf("known device came back as %q, want the same token", again)
	}

	// The device's own failures still back off, on its own counter.
	if status, _, _ := loginWith(t, srv.URL, map[string]string{"password": "typo", "device": device}); status != http.StatusUnauthorized {
		t.Fatalf("typo from the device = %d, want 401", status)
	}
	if status, _, _ := loginWith(t, srv.URL, map[string]string{"password": testPassword, "device": device}); status != http.StatusTooManyRequests {
		t.Fatalf("device login inside its own backoff = %d, want 429", status)
	}
}
