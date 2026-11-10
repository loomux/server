package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-268: session labels and the link flow's POST /api/v1/sessions
// (docs/design/tui-client-design.md, "The link flow").

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type sessionJSON struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	ClientKind string `json:"client_kind"`
	Current    bool   `json:"current"`
	Error      string `json:"error"`
	Code       string `json:"code"`
}

func listSessionRows(t *testing.T, base, token string) (int, []sessionJSON) {
	t.Helper()
	status, body := doJSON(t, http.MethodGet, base+"/api/v1/sessions", token, nil)
	var out struct {
		Sessions []sessionJSON `json:"sessions"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return status, out.Sessions
}

func linkSession(t *testing.T, base, token string, body any) (int, sessionJSON) {
	t.Helper()
	status, raw := doJSON(t, http.MethodPost, base+"/api/v1/sessions", token, body)
	var out sessionJSON
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("POST /sessions: %d %q: %v", status, raw, err)
	}
	return status, out
}

// The whole flow: the terminal's own token is refused until a logged-in
// session approves its hash, then it works, labelled, without the
// approver ever seeing it.
func TestLinkSession_Flow(t *testing.T) {
	h := newHoldServer(t)
	web := h.tokenA
	terminal := "a-token-the-terminal-made-itself"

	if status, _ := listSessionRows(t, h.base, terminal); status != http.StatusUnauthorized {
		t.Fatalf("the pending token before approval = %d, want 401", status)
	}
	status, made := linkSession(t, h.base, web, map[string]string{"token_hash": tokenHash(terminal), "label": "  laptop ", "client_kind": "tui"})
	if status != http.StatusCreated || made.ID == "" || made.Label != "laptop" || made.ClientKind != "tui" || made.Current {
		t.Fatalf("link = %d %+v", status, made)
	}
	status, sessions := listSessionRows(t, h.base, terminal)
	if status != http.StatusOK {
		t.Fatalf("the token after approval = %d, want 200", status)
	}
	var mine, approver *sessionJSON
	for i := range sessions {
		switch {
		case sessions[i].Current:
			mine = &sessions[i]
		case sessions[i].ID == h.sessionID(web):
			approver = &sessions[i]
		}
	}
	if mine == nil || mine.ID != made.ID || mine.Label != "laptop" || mine.ClientKind != "tui" {
		t.Errorf("the linked session in the list = %+v", mine)
	}
	// A login is the web's.
	if approver == nil || approver.ClientKind != "web" || approver.Label != "" {
		t.Errorf("the login session in the list = %+v; want kind web, no label", approver)
	}
	stored, err := h.store.GetSessionByTokenHash(context.Background(), tokenHash(terminal))
	if err != nil || stored.Origin != registry.SessionOriginLink {
		t.Errorf("stored linked session = %+v, %v; want origin link", stored, err)
	}
	if login, _ := h.store.GetSessionByTokenHash(context.Background(), tokenHash(web)); login == nil || login.Origin != registry.SessionOriginLogin {
		t.Errorf("stored login session = %+v; want origin login", login)
	}

	// The same hash again is a conflict, from anyone.
	if status, out := linkSession(t, h.base, h.tokenB, map[string]string{"token_hash": tokenHash(terminal)}); status != http.StatusConflict || out.Code != "conflict" {
		t.Errorf("the same hash again = %d %+v, want 409", status, out)
	}
	// A linked session can't link: a stolen terminal token mints nothing.
	status, out := linkSession(t, h.base, terminal, map[string]string{"token_hash": tokenHash("a-successor"), "client_kind": "tui"})
	if status != http.StatusForbidden || out.Code != "forbidden" {
		t.Errorf("a link from a linked session = %d %+v, want 403", status, out)
	}
	if status, _ := listSessionRows(t, h.base, "a-successor"); status != http.StatusUnauthorized {
		t.Errorf("the refused successor's token = %d, want 401", status)
	}

	// Revoking the linked session by its row ends it and nothing else.
	if status, _ := doJSON(t, http.MethodDelete, h.base+"/api/v1/sessions/"+made.ID, web, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d", status)
	}
	if status, _ := listSessionRows(t, h.base, terminal); status != http.StatusUnauthorized {
		t.Errorf("the revoked terminal token = %d, want 401", status)
	}
	if status, _ := listSessionRows(t, h.base, web); status != http.StatusOK {
		t.Errorf("the approver after revoking the terminal = %d, want 200", status)
	}
}

func TestLinkSession_Validation(t *testing.T) {
	h := newHoldServer(t)
	good := tokenHash("t")
	for name, body := range map[string]any{
		"no hash":          map[string]string{"label": "laptop"},
		"short hash":       map[string]string{"token_hash": good[:63]},
		"long hash":        map[string]string{"token_hash": good + "0"},
		"uppercase hash":   map[string]string{"token_hash": strings.ToUpper(good)},
		"not hex":          map[string]string{"token_hash": strings.Repeat("g", 64)},
		"the token itself": map[string]string{"token_hash": "a-raw-token-not-a-hash"},
		"label too long":   map[string]string{"token_hash": good, "label": strings.Repeat("x", 65)},
		"label control":    map[string]string{"token_hash": good, "label": "lap\x1b[31mtop"},
		"unknown kind":     map[string]string{"token_hash": good, "client_kind": "toaster"},
		"kind case":        map[string]string{"token_hash": good, "client_kind": "TUI"},
	} {
		if status, out := linkSession(t, h.base, h.tokenA, body); status != http.StatusBadRequest || out.Code != "invalid_request" || out.Error == "" {
			t.Errorf("%s = %d %+v, want 400", name, status, out)
		}
	}
	if status, out := linkSession(t, h.base, "", map[string]string{"token_hash": good}); status != http.StatusUnauthorized {
		t.Errorf("without a token = %d %+v, want 401", status, out)
	}
	if _, err := h.store.GetSessionByTokenHash(context.Background(), good); err == nil {
		t.Error("a refused link made a session")
	}
	// Label and kind may be left out; app and web are kinds too.
	for i, kind := range []string{"", "app", "web"} {
		status, out := linkSession(t, h.base, h.tokenA, map[string]string{"token_hash": tokenHash("ok-" + kind), "client_kind": kind})
		if status != http.StatusCreated || out.ClientKind != kind || out.Label != "" {
			t.Errorf("link %d with kind %q = %d %+v", i, kind, status, out)
		}
	}
}

// A session from before origins were recorded (origin empty, which only
// a store written by hand can have after the migration) may not link.
func TestLinkSession_NeedsALoginOrigin(t *testing.T) {
	h := newHoldServer(t)
	if err := h.store.CreateSession(context.Background(), &registry.Session{ID: "s-none", TokenHash: tokenHash("no-origin")}); err != nil {
		t.Fatal(err)
	}
	if status, out := linkSession(t, h.base, "no-origin", map[string]string{"token_hash": tokenHash("x")}); status != http.StatusForbidden {
		t.Errorf("a link from a session with no origin = %d %+v, want 403", status, out)
	}
}

// A hold says who has the task from the session: its client kind, and
// its label unless the takeover sends its own.
func TestTakeover_CopiesTheSessionsKindAndLabel(t *testing.T) {
	h := newHoldServer(t)
	terminal := "the-terminal-token"
	if status, out := linkSession(t, h.base, h.tokenA, map[string]string{"token_hash": tokenHash(terminal), "label": "laptop", "client_kind": "tui"}); status != http.StatusCreated {
		t.Fatalf("link = %d %+v", status, out)
	}
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	h.task("t2", registry.TaskStatusAwaitingInput, "")
	h.task("t3", registry.TaskStatusAwaitingInput, "")

	if status, out := h.post("t1/takeover", terminal, nil); status != http.StatusOK || out.Hold.ClientKind != "tui" || out.Hold.ClientLabel != "laptop" {
		t.Errorf("takeover by the linked terminal = %d %+v; want kind tui, label laptop", status, out.Hold)
	}
	if status, out := h.post("t2/takeover", terminal, map[string]string{"client_label": "laptop, window 2"}); status != http.StatusOK ||
		out.Hold.ClientKind != "tui" || out.Hold.ClientLabel != "laptop, window 2" {
		t.Errorf("takeover with its own label = %d %+v", status, out.Hold)
	}
	if status, out := h.post("t3/takeover", h.tokenA, nil); status != http.StatusOK || out.Hold.ClientKind != "web" || out.Hold.ClientLabel != "" {
		t.Errorf("takeover by a login session = %d %+v; want kind web, no label", status, out.Hold)
	}
}
