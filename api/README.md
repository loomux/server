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
- **Static SPA serving is opt-in and lives outside the API surface
  entirely** (LOOM-33). `WithStaticDir`/`LOOMUX_STATIC_DIR` (unset by
  default) points `Server` at a built web-client directory
  (`loomux/web`'s Vite output); any request whose path is neither `/api`
  nor starts with `/api/` is served from there, with fallback to
  `index.html` for anything that isn't a real file — a browser refresh on
  a client-side route like `/conversations/abc123` gets the SPA shell
  instead of a 404. Routed entirely in `ServeHTTP` ahead of `mux`,
  mirroring the existing `/api/` version check already there — `/api` and
  every `/api/*` path never reach the static handler (the bare `/api`
  path, with no trailing slash, is checked explicitly alongside the
  `/api/` prefix so it can't slip through and get served as static
  content) and the static handler never reaches `mux`, so neither can
  shadow the other. Not built as part of LOOM-23/LOOM-24 (the web client
  itself); this is the small server-side hosting addition their design
  flagged as a dependency.

## Errors

Every `/api/v1` error answer has the body `{error, code}`: `error` is
human-readable text (for display or logs; its wording may change), `code`
a stable machine-readable name a client branches on instead of parsing
the text. Both are always present. `writeError` (in `server.go`) sets the
default code for the status; `writeErrorCode` sets a specific one.

Default codes, by status:

| Status | `code` |
| --- | --- |
| 400 | `invalid_request` |
| 401 | `unauthorized` |
| 403 | `forbidden` |
| 404 | `not_found` |
| 409 | `conflict` |
| 413 | `too_large` |
| 422 | `unprocessable` |
| 429 | `rate_limited` (e.g. the login backoff) |
| 500 | `internal` |
| 501 | `not_implemented` |
| 502 | `bad_gateway` |
| 503 | `unavailable` |
| any other | `error` |

Specific codes, where a handler tells apart a case a client can act on:

| Code | Where |
| --- | --- |
| `conversation_busy` | `POST /api/v1/dispatch` `409` when the conversation already has a dispatch in flight; the body also carries that dispatch's `dispatch_id` |
| `idempotency_conflict` | `POST /api/v1/dispatch` `422` when the `Idempotency-Key` was already used for a different request |
| `unsupported_api_version` | `404` for any `/api` path outside `/api/v1`; the body also carries `supported_versions` |

New codes may be added within v1 (a client should treat an unknown code
like the status's default); a code once given to a case doesn't change.
A failed blocking dispatch's `500` is a dispatch job body, not this
envelope: it classifies the failure with `error_class`.

### API v1 conventions

- **Identifiers.** A resource's own id is `id`; a reference to another
  resource is `<thing>_id` (`workspace_id`, `target_id`, `task_id`).
  Three places predate the rule and keep their names for all of 1.x
  (API v1 freeze review, item 7): a dispatch's own id is `dispatch_id`
  (in `POST /dispatch`, `GET /dispatches/{id}`, a conversation's
  `dispatches[]` and the stream's `dispatch_update`); a row of
  `GET /conversations` names its conversation `conversation_id`, as a
  conversation has no other id; and the stream's `message_added`
  carries `message_id` (and `task_id`). Attach-info's `task_id` is the
  task asked about.
- **Enums** are snake_case (task statuses since the freeze review).
- **Errors** are `{error, code}` (see the error section).
- **`/web/*`** is an operations API outside the v1 stability promise
  (`docs/release/versioning.md`, "The API v1 contract").

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
  days), `LOOMUX_STATIC_DIR` (optional, default unset — static serving
  disabled).
- `server.go` — `Server` (implements `http.Handler`), `NewServer`,
  `Dispatcher`/`SessionStore` (the narrow seams this package depends on —
  satisfied by `*app.App` and `*app.App.Store()` respectively, without
  importing `app` directly, mirroring `router.RoutingModel`/
  `orchestrator.CompletionDetector`'s minimal-interface pattern), the
  `requireAuth` middleware, `APIVersion`, `WithStaticDir` (opt-in static
  SPA serving with `index.html` fallback, LOOM-33 — see Design above),
  and the handlers:
  - `POST /api/v1/login` — `{password}` → `{token}`
  - `POST /api/v1/logout` — auth-gated, revokes the presented token
  - `GET /api/v1/sessions` — auth-gated (LOOM-47), every active session
    (every device/client currently holding a valid, unexpired Bearer
    token), most-recently-used first: `{sessions: [{id, created_at,
    last_used_at, current}, ...]}`. `current` marks the session backing
    this request's own token, so a "your devices" UI can label one row
    without this API ever exposing a token or its hash. Wraps
    `SessionStore.ListSessions` and then applies the same
    `time.Since(last_used_at) > sessionTTL` expiry check `requireAuth`
    uses — `ListSessions` itself has no expiration concept (design spec:
    that's a decision for the layer that knows the configured TTL, same
    as `registry.Session.LastUsedAt`'s own doc comment says), and
    without this filter an expired row would still list as active until
    someone happened to present its token and trip `requireAuth`'s own
    opportunistic cleanup.
  - `DELETE /api/v1/sessions/{id}` — auth-gated (LOOM-47), revokes any
    session by id — the same underlying operation as `/logout`,
    generalized to any id from `GET /api/v1/sessions` (e.g. signing
    another device out remotely); revoking the current request's own
    session is allowed and behaves exactly like `/logout`. `204` on
    success, `404` if `id` doesn't name an existing session. Wraps
    `SessionStore.DeleteSession`.
  - `POST /api/v1/dispatch` — auth-gated, `{conversation_id?, message,
    workspace_hint?}`, optional `Idempotency-Key` header (≤ 255 chars).
    Since LOOM-80 every request becomes a **dispatch job** (wraps
    `Dispatcher`, i.e. `dispatch.Service`): the job and its user message
    are stored before anything runs, and the turn runs on a server-owned
    context, so a client that disconnects mid-turn loses nothing — the
    result lands on the job and in the conversation's history. See
    `docs/design/async-dispatch-design.md`.
    - **Blocking** (`?wait=true`): waits for the job and
      answers `200 {reply, dispatch_id, conversation_id, status, …}`, or
      `500 {error, error_class, dispatch_id, …}` if it failed or was
      interrupted.
    - **Async** (the default, or `Prefer: respond-async`): `202
      {dispatch_id, conversation_id, status}` at once, with `Location:
      /api/v1/dispatches/{id}` (and `Preference-Applied: respond-async`
      when the header asked). `?wait=true` with `Prefer: respond-async`
      is `400`. (`?async=true` was dropped before 1.0 and is ignored.)
    - The default is one constant, `dispatchAsyncByDefault` in
      `api/dispatch.go`: async since the LOOM-81 web shipped (it was
      blocking while the deployed web still expected `{reply}`).
    - `conversation_id` empty starts a new conversation (its id is in the
      response). A repeat with the same `Idempotency-Key` and the same
      body returns the original job and runs nothing; the same key with a
      different body is `422` (code `idempotency_conflict`). A conversation
      with a job still in flight answers `409 {error, code, dispatch_id}`
      (code `conversation_busy`) naming it (LOOM-83 owns
      serializing instead). `503` while the server is shutting down.
    - `workspace_hint` (LOOM-46) is optional — a client-supplied workspace
      ID (e.g. a chat UI already focused on that workspace's conversation)
      that's folded into the router model's prompt as advisory context
      only; the router model (design spec §6) keeps final authority over
      which workspace a message actually goes to.
  - `GET /api/v1/dispatches/{id}` — auth-gated (LOOM-80), one dispatch job:
    `{dispatch_id, conversation_id, status, reply?, error?, error_class?,
    created_at, started_at?, finished_at?}`, `status` one of `queued`,
    `running`, `succeeded`, `failed`, `interrupted` (shutdown or restart
    cut it off; error class `interrupted`); `404` if unknown.
  - `GET /api/v1/workspaces` — auth-gated (LOOM-19), lists registered
    workspaces sorted by name: `{workspaces: [{id, name, target_id,
    status, tags, description, rolling_summary,
    last_used_at?}, ...]}` (`is_dynamic` was removed before 1.0: every
    workspace is dynamic). Includes the router's matching metadata
    (tags/description/rolling_summary) and the usage
    timestamp now that the Phase 2 web fleet-status page consumes them
    (LOOM-44). Wraps `WorkspaceLister.ListWorkspaces`, a narrow seam
    mirroring `SessionStore`, satisfied structurally by `*app.App.Store()`.
  - `GET /api/v1/conversations` — auth-gated (LOOM-18), one summary row
    per distinct `conversation_id`, most-recently-updated first:
    `{conversations: [{conversation_id, workspace_id, status,
    updated_at, preview}, ...]}`. A conversation isn't a stored entity
    (no such table in the design spec) — this groups `TaskLister.ListTasks`
    by `conversation_id`, taking each conversation's most-recently-updated
    task as representative, since one conversation can span more than one
    task row (LOOM-13 continuation, or the router sending a later message
    in the same conversation to a different workspace), and adds every
    conversation in the message log via
    `MessageLister.ListConversationActivity` (LOOM-62). A conversation of
    only `answer_directly` turns has no task: it is listed with
    `workspace_id: ""` and `status: "completed"`. `updated_at` is the later
    of the latest task update and the latest message. `preview`
    (LOOM-45) is the conversation's first-ever message (via
    `MessageLister`, one lookup per distinct conversation), truncated to
    200 runes with a trailing "…" — the opening line for a conversation
    list UI row, deliberately the earliest message rather than the
    latest (status/updated_at already cover "what's happening now").
    `has_more` is `false` unless `?limit=` cut the list short (see
    "Lists and paging").
  - **Enums are snake_case** throughout the API (API v1 freeze review,
    item 9). Two were kebab-case before 1.0, and the store still spells
    them that way: task statuses (`running`, `awaiting_input`,
    `needs_attention`, `human_takeover`, `completed`, `failed`; a
    conversation's `status` is its latest task's) and a target's
    `permission_mode` (`auto`, `accept_edits`, `manual`; the legacy
    `accept-edits` is still accepted as input for 1.x). Agent-type names
    such as `claude-code` are identifiers, not enums, and keep their
    spelling.
  - `GET /api/v1/conversations/{id}` — auth-gated (LOOM-18), the full task
    history for one conversation plus its message transcript (LOOM-31):
    `{conversation_id, tasks: [{id, workspace_id, kind, agent_type,
    status, created_at, updated_at, started_at, completed_at}, ...],
    messages: [{id, role, content, task_id, dispatch_id?, created_at}, ...],
    dispatches: [{dispatch_id, status, reply?, error?, …}, ...]}` (dispatches
    and `dispatch_id` since LOOM-80; a conversation whose only turn is still
    in flight already resolves, via its job and stored user message), all
    oldest first; `404` only if there is neither task history nor any
    message for that `conversation_id` — an `answer_directly`-only
    conversation has messages but no tasks and still resolves to `200`.
    Backed by `TaskLister` (`ListTasks`, the store's unfiltered,
    cross-workspace task query — `ListTasksByWorkspace` alone can't answer
    "every task in this conversation" since a conversation isn't pinned to
    one workspace) and `MessageLister` (`ListMessagesByConversation`),
    both satisfied structurally by `*app.App.Store()`.
    `has_more` and `next_before` page `messages` with `?limit=` and
    `?before=<message id>`; without them every message comes back and
    `has_more` is `false` (see "Lists and paging").
  - `GET /api/v1/conversations/{id}/events` — auth-gated (LOOM-110), the
    conversation's dispatch audit trail, oldest first: `{conversation_id,
    events: [{id, dispatch_id?, created_at, kind, model?, tier?,
    target_id?, workspace_id?, task_id?, command?, outcome?, error_class?,
    duration_ms, detail?}, ...]}`. `kind` is `decision`, `command`,
    `provision`, `offer`, `offer_answered`, `agent_turn`, `relay` (late
    output) or `outcome`; commands are redacted like transcripts. An
    unknown conversation, or one past `LOOMUX_EVENT_RETENTION`, has an
    empty list. Backed by `EventStore` (`WithEvents`); `501` without it.
  - `GET /api/v1/conversations/{id}/stream` — auth-gated (LOOM-21),
    Server-Sent Events reporting task status transitions for one
    conversation, so a client can watch a dispatch progress instead of
    only getting a single reply when `POST /dispatch`'s blocking call
    eventually returns — that endpoint's own contract is unchanged, this
    is purely additive. No `404` for an unknown `conversation_id`: a
    client may open the stream before ever calling dispatch, to catch the
    very first transition. `event: task_update` frames carry
    `{task_id, workspace_id, status, updated_at}` for the conversation's
    most-recently-updated task, sent whenever that tuple changes. A `:
    heartbeat` comment line every 15s keeps the connection alive through
    the reverse-proxy deployment model (above); the stream stays open
    until the client disconnects or the request context ends — it does
    not auto-close on a terminal status, so a client can keep watching
    across multiple turns of a long-lived conversation. Implemented as a
    polling loop (default 500ms, `WithStreamPollInterval` override)
    against `TaskLister` — the same seam the conversation endpoints use —
    rather than a new push-based event bus, matching this repo's existing
    minimal-machinery style (e.g. completion detection's own idle-
    heuristic tier).

    Since LOOM-80 the same stream also carries `event: dispatch_update`,
    `data: {dispatch_id, status, reply?, error?, error_class?, updated_at}`,
    whenever one of the conversation's dispatch jobs changes. On connect it
    reports jobs still in flight (so a client reconnecting mid-turn picks
    up where it was) but not ones already finished. With the reply on the
    `succeeded` event, in the job and in the conversation's messages, an
    async client no longer has to hold a request open for the reply text.

    Since LOOM-121 it also carries `event: message_added`,
    `data: {message_id, task_id?, role, created_at}`, for each message
    logged to the conversation while the stream is open (messages there
    on connect aren't replayed). An agent can end its turn early — a job
    left running in the background — and report later; the server relays
    that report as an assistant message no dispatch carries, so this
    event is how a client learns to refetch the conversation.
  - `GET /api/v1/tasks/{id}/attach-info` — auth-gated (LOOM-20), resolves
    a task down to the target+session a human would SSH into to attach
    (design spec §4): `{task_id, tmux_session, target: {id, name, kind,
    host, user}}`; `404` if the task doesn't exist. Returns stored data
    as-is — no live probe of whether the tmux session is actually still
    up (matching this API's other endpoints' thin-passthrough style); a
    dead session is discovered the same way a human always would, by
    trying to attach. Wraps `AttachInfoStore` (`GetTask` → `GetWorkspace`
    → `GetTarget`), a narrow seam mirroring the others here.
  - Targets carry `relay` (`full`, `last_message`, `none`, or `""` for
    the purpose's default: `none` for `purpose: work`, else `full`) and
    show `relay_effective`: what the router models may see of the
    target's work (`docs/deploy/operations.md`, "What the router models
    see").
  - `POST /api/v1/targets/{id}/scan-host-key`,
    `POST /api/v1/targets/{id}/pin` `{fingerprint}`,
    `DELETE /api/v1/targets/{id}/pin`, `POST /api/v1/targets/{id}/test` —
    auth-gated target onboarding (LOOM-114, `WithHostKeyPinning`; 501
    without it). A scan returns `{target_id, host_keys: [{type,
    fingerprint}], expires_at, pinned_host_keys}` and trusts nothing; `502`
    with the SSH hint when the host can't be read. A pin must name a
    fingerprint from the target's latest scan, at most 10 minutes old
    (`409` otherwise), and returns the target, whose `pinned_host_keys`
    then lists it. A pinned target is checked against its pin alone. Only
    remote targets (`400` for local). `test` returns `{target_id, reachable,
    tmux_version?, latency_ms, error?, host_key_problem}`. Targets also
    take and show `ssh_port` (0: the SSH config's). Single user today;
    scanning and pinning must become admin-only if Loomux ever has
    several (see `docs/deploy/ssh.md`).
  - `POST /api/v1/targets`, `GET /api/v1/targets`,
    `PUT /api/v1/targets/{id}`, `DELETE /api/v1/targets/{id}` —
    auth-gated target registration (LOOM-59). `registry.Store`'s target
    CRUD existed and was tested from the first commit but had *no
    production caller at all*, so the only way to register an execution
    target was to hand-write a row into sqlite on the deployment host.
    That left a fresh deployment with nowhere to dispatch work — the
    router correctly falls back to `answer_directly` when no target
    exists — and it is why these are HTTP routes rather than an admin
    CLI subcommand: registration has to be reachable by an operator who
    has no shell on the pod, and a CLI would have been a second
    validation path to keep in sync while still not meeting that bar.
    The deployment already configures `LOOMUX_AUTH_PASSWORD_HASH`, so
    there is no bootstrap chicken-and-egg to justify one.

    Bodies are `{name, kind, host, user}` plus the optional fields below;
    responses add
    the server-minted `id` plus `created_at`/`updated_at`. **The id is
    never accepted from the client** — workspaces reference targets by
    id, and letting a caller choose one invites exactly the collisions
    and hand-minted ids this endpoint exists to replace, so it is
    generated here the way session ids already are. `ssh_key_ref` is not
    part of the API (removed before 1.0, freeze review item 1: nothing
    read it; SSH keys come from the mounted secret); a client that still
    sends it has it ignored.

    Validation enforces the invariants the execution layer assumes but
    cannot check at registration time, so a target that could never be
    dispatched to is rejected at the boundary rather than stored and
    discovered broken later: `kind` must be one of the two
    `targets.NewExecutor` knows; a `remote` needs both `host` and
    `user`, because `RemoteExecutor.destination()` builds `user+"@"+host`
    and ssh rejects a bare `@host`; a `local` must carry neither, since
    clearing them silently would hide a caller's misunderstanding until
    an attach-info response came back missing fields they thought they
    had set. Bad body or failed validation is `400`, a duplicate name
    `409` (the store's unique constraint).

    All four operations ship, not just create, because create-only would
    reproduce this ticket's own failure mode in miniature — a write path
    with no way to read or fix. `GET` is how a client discovers the
    server-assigned id it needs to attach a workspace; `DELETE` undoes a
    mistaken registration (`409`, not `500`, once workspaces reference
    it — the foreign key doing its job, and the operator needs to be
    told which it is); `PUT` is then the *only* correction path left,
    which is why it exists. `PUT` re-reads the stored row rather than
    echoing back the struct it passed to `UpdateTarget`: that call fills
    in `UpdatedAt` but not `CreatedAt`, so only a read returns a
    canonical row. Unknown id is `404` on both.
  - `GET /api/v1/version` — unauthenticated, `{server_version, api_version}`
    (`server_version` comes from the top-level `version` package, not
    defined in this one)

## Lists and paging

A list that can grow without bound carries `has_more` (a boolean, always
present) beside it, so paging can be introduced without breaking a
client that assumed the list was complete. Today that is
`GET /api/v1/conversations` (`conversations`), `GET
/api/v1/conversations/{id}` (`messages`), `GET
/api/v1/conversations/{id}/events` (`events`, not paged yet: always
`false`) and `GET /api/v1/tasks/{id}/transcript` (`turns`). Clients must
check `has_more`: `false` means the list is complete, `true` that more
remain.

The other lists are **complete lists** for all of v1: `GET /workspaces`,
`/targets`, `/credentials`, `/sessions` and `/targets/{id}/agents`,
bounded in practice by what one person registers. They carry no
`has_more`, and paging them would need new, opt-in parameters.

Paging follows the task transcript's convention (LOOM-122):

- `?limit=N` returns at most N items. Out of range (not a number, below
  1, above the route's maximum) is `400`.
- Items stay in the list's own order. For a list that grows at the end
  (messages, transcript turns: oldest first), a page is the *latest* N,
  and `next_before` (present when `has_more` is true) is the `before=`
  value that fetches the page preceding it: `?before=<id>&limit=N`. A
  `before` naming no item of the list is `400`.
- The transcript always pages (default 20, at most 100: a turn carries a
  pane). The conversation routes have **no default limit** (at most 500
  when given): without `limit` they return everything and `has_more` is
  `false`, as before the field existed. Tasks, dispatches and
  confirmations in `GET /conversations/{id}` are always complete; only
  `messages` pages. `GET /conversations` (most recently updated first)
  takes `limit` but no cursor yet: its order moves with activity, so a
  client wanting more asks for a larger `limit`.

## Testing

`server_test.go` covers the auth/session state machine (login success/
failure, missing/invalid/expired/logged-out tokens, sliding-expiration
refresh, and the login throttle — repeated failures triggering `429`
with `Retry-After`, a success resetting it, that it's shared across
different claimed `X-Forwarded-For` values (proving "global" is real,
not just documented), and that enough waiting always lets the correct
password through no matter how many failures preceded it), plus
`GET /api/v1/sessions` / `DELETE /api/v1/sessions/{id}` (LOOM-47: listing
across multiple logins with exactly the caller's own session marked
`current`, a session past `sessionTTL` excluded from the listing even
though nothing has presented its token since expiry to trigger
`requireAuth`'s own opportunistic cleanup, revoke-by-id invalidating
that token without touching others, unknown-id `404`, re-revoking the
same id `404` rather than a repeatable success, and revoking the
current session behaving like `/logout`)
against a fake `Dispatcher` and a real (temp-file) sqlite store for sessions, plus
`/api/v1/workspaces` (auth required, empty-list, populated-and-sorted,
 and metadata round-trip cases, asserting the full field set including
 tags/description/rolling_summary/last_used_at
 against real store fixtures) and
`/api/v1/conversations` + `/api/v1/conversations/{id}` (auth required,
empty-list, recency-sorted grouping across multiple conversations, a
direct-answer-only (task-less) conversation being listed and recency
counting messages as well as tasks (LOOM-62), a
conversation spanning two workspaces returning chronological history,
an unrelated conversation not leaking in, and unknown-id 404) and
`/api/v1/tasks/{id}/attach-info` (auth required, unknown-task 404, and a
remote target's host/user/kind flowing through correctly), and
`/api/v1/conversations/{id}/stream` (auth required; a real SSE round-trip
— created-then-updated task status changes arriving as ordered
`task_update` events, parsed off the live response body with a bounded
per-event timeout so a broken stream fails the test instead of hanging).
Run with `-race` too — the stream handler's poll loop and heartbeat
ticker run concurrently with request handling, and this is the one
handler in this package doing that.
`server_test.go` also covers `WithStaticDir` (LOOM-33): a real static
file served as-is, an unknown client-side route and the root path both
falling back to `index.html`, `/api/*` paths staying untouched by static
serving (including the unsupported-API-version rejection still winning
over the SPA fallback, and the bare `/api` path with no trailing slash
specifically — a prior version of this routing check missed it and let
it fall through to the static handler), a path-traversal attempt (both
literal `..` and its `%2e%2e` percent-encoded form) falling back to the
SPA shell rather than escaping the configured directory, and —
separately — that static serving stays off (still `404`) when
`WithStaticDir` is never used, matching this
package's behavior before the option existed.
`targets_test.go` covers the LOOM-59 target endpoints end-to-end through
real HTTP against a real store: a remote target created and read back by
`GET`, a local one persisting without host/user, server-minted ids being
distinct and a client-supplied `id` field being ignored rather than
honoured, every validation rejection (missing name; unknown/empty/
wrong-case `kind`; remote missing host or user; local carrying either;
malformed JSON) as `400` with a populated error envelope, duplicate name
as `409`, `PUT` changing stored fields and `DELETE` removing them,
unknown id as `404` on both, `DELETE` of a target with workspaces
attached as `409`, and — the check that matters most for an
administrative endpoint — that all four routes reject an unauthenticated
caller *and* that a rejected `POST` leaves the store empty, so auth
failure can't be a write that merely reports failure. The unknown-id
cases assert on the error envelope specifically: net/http's mux also
returns `404` for an unregistered route, so a bare status check there
passes vacuously whether or not the handler exists.
`throttle_test.go` covers `loginThrottle` in isolation (pure timing
logic, including the zero-base edge case tests use to disable backoff
entirely). `integration_test.go` proves the auth/dispatch behavior
against a real `app.App` (real sqlite, a real `llmrouter.Model` hitting
an `httptest.Server` standing in for the LLM vendor) driven purely over
real HTTP — the full stack, not just this package in isolation. Run
`go test ./api/...`.
