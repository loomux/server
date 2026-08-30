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
}
