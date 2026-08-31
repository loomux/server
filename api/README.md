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
- **`/login` brute-force protection: global exponential backoff, not a
  hard lockout** (LOOM-15). `throttle.go`'s `loginThrottle` is one
  in-memory counter for the whole process — deliberately *not* scoped
  per-IP or per-user: there's no username here to scope by (`loginRequest`
  is just `{password}` — single-user, single credential), and behind the
  confirmed reverse-proxy deployment model every request's `RemoteAddr`
  is typically the proxy's own address anyway, so IP-scoping would either
  do nothing (using `RemoteAddr`) or require trusting a client-spoofable
  `X-Forwarded-For` header — a global counter sidesteps that trust
  question entirely. Backoff, not a lockout, is also deliberate: on a
  single-account system a hard lockout is itself a denial-of-service
  vector (an attacker who can't guess the password can still lock the
  real user out just by failing repeatedly). Each failure doubles the
  wait before the *next* attempt is even checked (1s, 2s, 4s, ... capped
  at 30s); a success resets it to zero. A throttled attempt gets `429`
  with `Retry-After` — including a *correct* password presented while
  throttled, so the throttle state itself never leaks whether a guess
  would have worked. Still no audit log of failed attempts (no logging
  convention in this repo to hook into yet), and state resets on
  restart (in-memory only) — acceptable since the confirmed deployment
  has no attacker-triggerable restart path.
- **API version mismatch handling is real but URL-only** (LOOM-10): any
  `/api/...` path outside `/api/v1/` — a future `/api/v2/`, a typo, the
  bare `/api/` root — gets a structured 404 naming `api.APIVersion`
  (`ServeHTTP`, ahead of routing so it can never shadow a real `/api/v1/`
  route hit with the wrong HTTP method, which correctly gets ServeMux's
  own 405 instead). `GET /api/v1/version` (unauthenticated) lets a client
  check `api.APIVersion` + `version.Version` before it even logs in. This
  satisfies §10 axis 1's "a mismatch is a clear rejection... not silent
  breakage" for the one version that exists — there's still no
  negotiation protocol beyond the URL itself (e.g. a client declaring a
  minimum/maximum it accepts), since nothing has needed one with only a
  v1 to compare against.
- **No idle-session reaper.** A session past its TTL is only actually
  deleted the next time someone tries to use it (opportunistic cleanup in
  `requireAuth`) — an abandoned expired row otherwise just sits in the
  table. Harmless bloat for a single-user table, same shape as LOOM-16's
  idle-conversation-reaper gap.

## Layout

- `auth.go` — `HashPassword` (bcrypt, used by `loomuxd -hash-password`
  and by callers configuring `LOOMUX_AUTH_PASSWORD_HASH`), `checkPassword`,
  token generation/hashing (`newToken`, `hashToken`).
- `throttle.go` — `loginThrottle`: the global exponential-backoff counter
  behind `/login` (see Design above for why global/backoff, not
  per-IP/lockout).
- `config.go` — `Config`, `LoadConfig()`: `LOOMUX_AUTH_PASSWORD_HASH`
  (required, validated as a real bcrypt hash), `LOOMUX_HTTP_ADDR`
  (optional, default `:8080`), `LOOMUX_SESSION_TTL` (optional, default 30
  days).
- `server.go` — `Server` (implements `http.Handler`), `NewServer`,
  `Dispatcher`/`SessionStore` (the narrow seams this package depends on —
  satisfied by `*app.App` and `*app.App.Store()` respectively, without
  importing `app` directly, mirroring `router.RoutingModel`/
  `orchestrator.CompletionDetector`'s minimal-interface pattern), the
  `requireAuth` middleware, `APIVersion`, and the four handlers:
  - `POST /api/v1/login` — `{password}` → `{token}`
  - `POST /api/v1/logout` — auth-gated, revokes the presented token
  - `POST /api/v1/dispatch` — auth-gated, `{conversation_id, message}` →
    `{reply}`, wraps `Dispatcher.Dispatch`
  - `GET /api/v1/version` — unauthenticated, `{server_version, api_version}`
    (`server_version` comes from the top-level `version` package, not
    defined in this one)

## Testing

`server_test.go` covers the auth/session state machine (login success/
failure, missing/invalid/expired/logged-out tokens, sliding-expiration
refresh, and the login throttle — repeated failures triggering `429`
with `Retry-After`, a success resetting it, that it's shared across
different claimed `X-Forwarded-For` values (proving "global" is real,
not just documented), and that enough waiting always lets the correct
password through no matter how many failures preceded it) against a fake
`Dispatcher` and a real (temp-file) sqlite store for sessions.
`throttle_test.go` covers `loginThrottle` in isolation (pure timing
logic, including the zero-base edge case tests use to disable backoff
entirely). `integration_test.go` proves the auth/dispatch behavior
against a real `app.App` (real sqlite, a real `llmrouter.Model` hitting
an `httptest.Server` standing in for the LLM vendor) driven purely over
real HTTP — the full stack, not just this package in isolation. Run
`go test ./api/...`.
