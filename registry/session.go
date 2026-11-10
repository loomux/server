package registry

import "time"

// Session is a client's authenticated bearer-token session (design spec
// §9: "real login-gated auth"). TokenHash is a SHA-256 hex digest of the
// raw bearer token the client holds — the raw token itself is never
// persisted anywhere, only ever returned to the client once at login, so
// a database compromise alone doesn't hand out working credentials.
type Session struct {
	ID        string
	TokenHash string
	CreatedAt time.Time
	// LastUsedAt drives sliding-expiration: a caller (api package) treats
	// a session as expired once too much time has passed since this was
	// last updated, and refreshes it (TouchSession) on every authenticated
	// request. registry itself has no expiration policy — that's a
	// decision for the layer that knows the configured TTL, matching how
	// task/workspace status transitions are decided by orchestrator/router,
	// not by registry.
	LastUsedAt time.Time
	// Label names the device the session is on ("laptop"), ClientKind
	// the client holding it (a SessionKind value, or empty when unknown),
	// both for a person choosing which session to revoke (LOOM-268).
	Label      string
	ClientKind string
	// Origin is how the session was made: SessionOriginLogin or
	// SessionOriginLink. Only a login session may approve a link, so a
	// stolen linked token can't mint itself successors. Empty is treated
	// as not a login.
	Origin string
}

// How a session was made (Session.Origin).
const (
	// SessionOriginLogin: with the password, at POST /api/v1/login.
	SessionOriginLogin = "login"
	// SessionOriginLink: approved from a login session, which never saw
	// its token (the terminal's link flow, POST /api/v1/sessions).
	SessionOriginLink = "link"
)

// The clients a session can say it is (Session.ClientKind).
const (
	SessionKindWeb = "web"
	SessionKindTUI = "tui"
	SessionKindApp = "app"
)

// ValidSessionKind reports whether kind is a known client kind, or empty
// (unknown).
func ValidSessionKind(kind string) bool {
	switch kind {
	case "", SessionKindWeb, SessionKindTUI, SessionKindApp:
		return true
	}
	return false
}
