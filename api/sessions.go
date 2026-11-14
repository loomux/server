package api

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// linkSessionRequest asks for a session for a token the caller never
// sees (LOOM-268).
type linkSessionRequest struct {
	// TokenHash is hex(sha256(token)), 64 lowercase hex digits: what the
	// server stores for every session. The token itself stays with the
	// client that made it.
	TokenHash string `json:"token_hash"`
	// Label names the new session's device ("laptop"); at most 64
	// characters, no control characters.
	Label string `json:"label,omitempty"`
	// ClientKind is the client the session is for: tui, app, web, or
	// left out.
	ClientKind string `json:"client_kind,omitempty"`
}

var tokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newSessionSummary(sess, current *registry.Session) sessionSummary {
	return sessionSummary{
		ID:         sess.ID,
		CreatedAt:  sess.CreatedAt,
		LastUsedAt: sess.LastUsedAt,
		Label:      sess.Label,
		ClientKind: sess.ClientKind,
		Current:    current != nil && sess.ID == current.ID,
	}
}

// handleLinkSession serves POST /api/v1/sessions (LOOM-268,
// docs/design/tui-client-design.md "The link flow"): a logged-in session
// approves a new session for a token another client generated, by that
// token's hash. A terminal behind the deployment's auth proxy can't reach
// the login and must never hold the password; it makes its own token,
// shows the hash to the browser, and polls with the token until this call
// makes the session exist.
//
// Only a session made by a login may do this: a linked session's token,
// if stolen, can't mint itself successors that outlive its revocation.
// The new session's origin is link, and it has the same lifetime as any
// other. 409 if a session already has the hash.
func (s *Server) handleLinkSession(w http.ResponseWriter, r *http.Request) {
	var req linkSessionRequest
	if !readJSON(w, r, &req) {
		return
	}
	caller := sessionFromContext(r.Context())
	if caller.Origin != registry.SessionOriginLogin {
		writeError(w, http.StatusForbidden, "only a session that logged in with the password can link another")
		return
	}
	if !tokenHashPattern.MatchString(req.TokenHash) {
		writeError(w, http.StatusBadRequest, "token_hash must be 64 lowercase hex digits")
		return
	}
	label, ok := cleanClientLabel(req.Label)
	if !ok {
		writeError(w, http.StatusBadRequest, "label must be at most 64 characters, with no control characters")
		return
	}
	if !registry.ValidSessionKind(req.ClientKind) {
		writeError(w, http.StatusBadRequest, `client_kind must be "tui", "app" or "web"`)
		return
	}
	sess := &registry.Session{
		ID:         uuid.NewString(),
		TokenHash:  req.TokenHash,
		LastUsedAt: s.now().UTC(),
		Label:      label,
		ClientKind: req.ClientKind,
		Origin:     registry.SessionOriginLink,
	}
	err := s.sessions.CreateSession(r.Context(), sess)
	switch {
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, "a session already exists for this token")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not create session")
	default:
		writeJSON(w, http.StatusCreated, newSessionSummary(sess, caller))
	}
}
