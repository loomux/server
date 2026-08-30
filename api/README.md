# api

The client-facing HTTP surface (design spec §9, §10 axis 1): real,
login-gated Bearer-token auth in front of `app.App.Dispatch` — "there is
no 'it's just me so no auth' shortcut" (§9), since web/Android/iOS clients
are reachable outside the server process's own trust boundary.

## Design

Confirmed with the user before building (security/deployment tradeoffs,
not engineering taste):

- **Deployment model: plain HTTP behind a reverse proxy.** `Server`
  serves plain `http.Handler` — it does not terminate TLS itself. The
  expectation is a reverse proxy (Caddy/nginx) or an overlay network
  (Tailscale) in front handling TLS; `loomuxd` binds to whatever
  `LOOMUX_HTTP_ADDR` says (default `:8080`) with no cert/key story of its
  own. Running `loomuxd` directly exposed to an untrusted network over
  plain HTTP would leak the password and bearer tokens in transit — that's
  the proxy's job to prevent, not this package's.
- **Auth mechanism: password login → opaque Bearer session token**, not
  cookies (native mobile clients don't do cookies well) and not a static
  API key (no session concept, no per-device revocation, "here's the
  proof you don't cut corners" — spec §9's own framing — points at a real
  login flow). `POST /api/v1/login` checks a bcrypt hash
  (`LOOMUX_AUTH_PASSWORD_HASH`, generate with `loomuxd -hash-password`) —
  the same env-at-process-start convention as the router's own credential
  (LOOM-12) and the vault's master key (LOOM-7), since v1 is single-user.
  On success, a random 32-byte token (`crypto/rand`) is returned to the
  client exactly once; only its SHA-256 hash is ever persisted
  (`registry.Session.TokenHash`) — a database compromise alone doesn't
  hand out working credentials. Every other request sends it as
  `Authorization: Bearer <token>`.
  - The user's stated plan is SSO/OAuth later. The seam for that is the
    session layer itself, not a shared `Authenticator` interface: however
    a future auth method proves identity (an OAuth callback, say), it
    ends the same way this one does — mint a token, `CreateSession`,
    return it to the client. `checkPassword` (auth.go) is the one thing
    that would get an alternative, not the session/middleware machinery
    around it.
- **Session lifetime: long sliding expiration + explicit logout.** A
  session stays valid as long as it's used at least once within
  `SessionTTL` (default 30 days, `LOOMUX_SESSION_TTL` overridable,
  `time.ParseDuration` syntax) — `requireAuth` refreshes `LastUsedAt` on
  every authenticated request. `POST /api/v1/logout` deletes the session
  row outright, for revoking a lost device immediately rather than
  waiting out the window.

## Known gaps (flagged, not built)

- **No brute-force protection on `/login`** beyond bcrypt's own
  computational cost. No lockout, no rate limiting, no audit log of
  failed attempts (this repo has no logging convention to hook into yet).
  A reverse proxy in front (the assumed deployment model above) commonly
  covers this (fail2ban, Cloudflare, etc.) — but `loomuxd` itself doesn't
  enforce anything. Worth a future ticket if this is ever internet-facing
  without such a proxy.
- **API versioning is just the `/api/v1/...` prefix** — there's only ever
  been one version so far, so the spec's "a mismatch is a clear rejection
  or warning" requirement (§10 axis 1) hasn't been exercised against a
  real v2. A request to an undefined path 404s, which is "a clear
  rejection," but no version-negotiation protocol (e.g. a client
  declaring what it expects beyond the URL) exists yet.
- **No idle-session reaper.** A session past its TTL is only actually
  deleted the next time someone tries to use it (opportunistic cleanup in
  `requireAuth`) — an abandoned expired row otherwise just sits in the
  table. Harmless bloat for a single-user table, same shape as LOOM-15's
  idle-conversation-reaper gap.

## Layout

- `auth.go` — `HashPassword` (bcrypt, used by `loomuxd -hash-password`
  and by callers configuring `LOOMUX_AUTH_PASSWORD_HASH`), `checkPassword`,
  token generation/hashing (`newToken`, `hashToken`).
- `config.go` — `Config`, `LoadConfig()`: `LOOMUX_AUTH_PASSWORD_HASH`
  (required, validated as a real bcrypt hash), `LOOMUX_HTTP_ADDR`
  (optional, default `:8080`), `LOOMUX_SESSION_TTL` (optional, default 30
  days).
- `server.go` — `Server` (implements `http.Handler`), `NewServer`,
  `Dispatcher`/`SessionStore` (the narrow seams this package depends on —
  satisfied by `*app.App` and `*app.App.Store()` respectively, without
  importing `app` directly, mirroring `router.RoutingModel`/
  `orchestrator.CompletionDetector`'s minimal-interface pattern), the
  `requireAuth` middleware, and the three handlers:
  - `POST /api/v1/login` — `{password}` → `{token}`
  - `POST /api/v1/logout` — auth-gated, revokes the presented token
  - `POST /api/v1/dispatch` — auth-gated, `{conversation_id, message}` →
    `{reply}`, wraps `Dispatcher.Dispatch`

## Testing

`server_test.go` covers the auth/session state machine (login success/
failure, missing/invalid/expired/logged-out tokens, sliding-expiration
refresh) against a fake `Dispatcher` and a real (temp-file) sqlite store
for sessions. `integration_test.go` proves the same behavior against a
real `app.App` (real sqlite, a real `llmrouter.Model` hitting an
`httptest.Server` standing in for the LLM vendor) driven purely over real
HTTP — the full stack, not just this package in isolation. Run
`go test ./api/...`.
