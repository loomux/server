# Loomux — Web Client Design (LOOM-23)

## Overview

This is Phase B's first client: a browser-based single-page app that is the
primary way a human drives Loomux day-to-day — log in, see registered
workspaces, hold conversations with the agent fleet, watch a dispatch
progress live, and get the attach-info needed to `ssh` + `tmux attach` for a
manual takeover. It plays the same role for Phase B that the original core
design (`core-design.md`) plays for Phase A: it formalizes and replaces a
manual workflow (in this case, curl-ing `loomuxd` directly) rather than
inventing new server behavior.

This spec covers the client only. The server-side API it consumes
(`docs/design/core-design.md` §9, §10 axis 1) already exists and is treated
here as a fixed contract — see "API surface consumed," below, for the exact
shapes.

## Scope

**In scope for this design:**
- Framework and build tooling
- Auth flow (login, token storage, session handling)
- Page/route structure
- The data layer against the existing API surface
- Real-time updates (SSE)
- Hosting/serving model
- Error handling
- Testing strategy

**Explicitly out of scope** (each is either a separate future spec or
already filed as its own ticket):
- Android/iOS clients (LOOM-26, LOOM-30 — own design passes)
- SSO/OAuth (the API's own design already names this as a planned future
  swap of `checkPassword` alone, per `api/README.md` — this client's auth
  layer is built with that seam in mind but does not implement it)
- GitHub-provisioning UI (LOOM-28, LOOM-29)
- Multi-user support (the server itself is single-user by design, per
  `core-design.md` §9)

## API surface consumed

All under `/api/v1/`, Bearer-token auth except `/login` and `/version`
(`api/server.go`, current as of this repo's `main` at the time of writing):

- `POST /login` — `{password}` → `{token}`
- `POST /logout` — auth'd, revokes the presented token
- `POST /dispatch` — auth'd, `{conversation_id, message}` → `{reply}`
  (blocking by default). Since LOOM-80 the turn runs as a server-side job
  either way, and `Prefer: respond-async` makes it answer `202
  {dispatch_id, conversation_id, status}` at once instead; results then
  come from `GET /dispatches/{id}`, the stream's `dispatch_update` events
  and the conversation's history. The web moves to async in LOOM-81 — see
  `docs/design/async-dispatch-design.md`, "Web impact".
- `GET /workspaces` — auth'd, `{workspaces: [{id, name, target_id,
  status, tags, description, capabilities, rolling_summary, is_dynamic,
  last_used_at?}, ...]}`
- `GET /conversations` — auth'd, `{conversations: [{conversation_id,
  workspace_id, status, updated_at}, ...]}`, most-recently-updated first
- `GET /conversations/{id}` — auth'd, `{conversation_id, tasks: [{id,
  workspace_id, kind, agent_type, status, created_at, updated_at,
  started_at?, completed_at?}, ...]}`, oldest first, `404` if unknown
- `GET /conversations/{id}/stream` — auth'd, SSE, `event: task_update` /
  `data: {task_id, workspace_id, status, updated_at}` whenever the
  conversation's latest task's `(id, status, updated_at)` changes; `:
  heartbeat` comments every 15s
- `GET /tasks/{id}/attach-info` — auth'd, `{task_id, tmux_session, target:
  {id, name, kind, host, user}}`, `404` if unknown
- `GET /version` — unauthenticated, `{server_version, api_version}`

### Persisted message transcript (LOOM-31 — shipped)

`GET /conversations/{id}` returns a `messages` array (per-turn transcript,
oldest first — `{id, role, content, task_id, created_at}`) alongside the
task-lifecycle rows, landed server-side in LOOM-31 and wired into this
client in `loomux/web#1` (merged 2026-09-07). `history.messages` is the
chat view's source of truth for persisted turns; the only client-side
state is a short-lived optimistic entry for the turn currently in flight
(the just-sent message + a pending reply while `POST /dispatch` is
outstanding), cleared once the post-dispatch refetch lands. Reopening an
older conversation, or opening one from a second device, now shows the
real prior message text, not just the task-history skeleton.

## Architecture Overview

```
┌─────────────────────────┐
│   Browser (SPA)          │
│   React + Vite build     │
│   served as static files │
└─────────────┬────────────┘
              │ HTTPS (reverse proxy terminates TLS —
              │ same deployment assumption as api/README.md)
              ▼
┌─────────────────────────┐
│        loomuxd            │
│  /api/v1/*  → api.Server   │  (existing)
│  /*         → static files │  (new — see "Hosting," below)
└─────────────────────────┘
```

Same-origin: the client is served by the same `loomuxd` process (behind the
same reverse proxy) that serves the API. No CORS story is needed.

## Components

### Framework and tooling

**Vite + React 19 + TypeScript.** Not a meta-framework with SSR (Next.js's
or SvelteKit's default mode) — that requires a Node process at runtime,
which contradicts "served by loomuxd as static files." Vite's `build`
output is plain HTML/CSS/JS: trivial for a Go binary to serve, no separate
Node deploy target. React over an alternative (e.g. Svelte) specifically
because this codebase will most likely be extended across future sessions
by Claude Code agents, the same way this server repo has been — React's
ecosystem size and training-data coverage make that more reliable than a
smaller framework's leaner-but-less-common patterns would be worth.

Supporting stack:
- **React Router** — a handful of routes, nothing more is needed
- **TanStack Query** — caching/loading/retry state for the list endpoints
  (`/workspaces`, `/conversations`), instead of hand-rolling it
- **`@microsoft/fetch-event-source`** for SSE — not native `EventSource`:
  caught during scaffolding that `EventSource` cannot send a custom
  `Authorization` header (only cookie auth), and
  `/conversations/{id}/stream` is Bearer-token auth-gated via
  `requireAuth` like every other endpoint. This small `fetch`-based
  library sends the same headers as any other API call and handles
  reconnection itself — avoids a server-side change (e.g. accepting the
  token as a query param) to keep the header-based auth model uniform
  across every endpoint
- **Tailwind CSS** for styling
- **Vitest + React Testing Library** for tests

### Auth flow

`/login` posts `{password}` → stores the returned bearer token in
`localStorage` → an `AuthProvider` attaches `Authorization: Bearer <token>`
to every request via a thin fetch wrapper. Token in `localStorage` (not
memory-only) so a page reload doesn't force re-login, matching the API's
own 30-day sliding-expiration design intent (`api/README.md`) — this is a
personal single-user tool checked from a phone throughout the day, not a
target where XSS-hardened token storage is the dominant risk.

A `401` from any call clears the stored token and redirects to `/login` —
covers both real logout-elsewhere and the sliding TTL lapsing. There is no
separate "session expired" detection beyond this; the server is the source
of truth for validity.

`GET /version` is called unauthenticated on app boot to catch a stale
client / mismatched API version early (spec's own §10 axis 1 intent
extended to this client) — a persistent banner, not a hard block, since
`v1` is the only version that exists today.

### Pages / routes

- `/login` — password form
- `/workspaces` — list: id, name, target, status, tags, description,
  capabilities, rolling_summary, is_dynamic, last_used_at (mirrors
  `workspaceSummary` exactly, no client-side enrichment)
- `/conversations` — list, most-recently-updated first (mirrors
  `conversationSummary`)
- `/conversations/:id` — the chat view: message composer, live status via
  SSE, task history, and attach-info (see below) surfaced per task
- New conversation: composing a message with no existing `conversation_id`
  simply generates a fresh UUID client-side and dispatches with it — the
  server has no "create conversation" endpoint, since a conversation isn't
  a stored entity (`core-design.md`/`api/README.md`) — it's implicit in
  shared `conversation_id`s across tasks

### Attach-info surfacing

When a task's status implies a human might want to intervene (`running`,
`awaiting-input`, `human-takeover`), its card in the chat view exposes a
"attach" affordance that calls `GET /tasks/{id}/attach-info` and displays
the resolved `ssh <user>@<host>` + `attach_command` (`tmux -L loomux attach -t <session>`, LOOM-93) command as
copyable text. The client never attempts to open an SSH connection itself
— purely informational, matching the design spec's own attach model (§4):
attaching is something a human does directly against the target, outside
Loomux's client/server boundary entirely.

### Real-time updates

`fetchEventSource` (`@microsoft/fetch-event-source`) against
`/conversations/{id}/stream`, with the same `Authorization: Bearer <token>`
header as every other call, drives a live status indicator while a `POST
/dispatch` call is in flight or a task from a prior turn is still
resolving. The dispatch call's own blocking response remains the sole
source of actual reply text (matches the API's documented contract exactly
— see "Persisted message transcript," above, and the stream is
supplementary, never a replacement per `api/README.md`'s own design note).
The library handles
reconnection on drop itself; no custom reconnect logic is written.

## Hosting / serving integration

**Shipped (LOOM-33, `loomux/server#34`, merged 2026-09-07).** `loomuxd` now
serves a configured static directory (`LOOMUX_STATIC_DIR`) for any
non-`/api/*` path, with SPA fallback to `index.html` for client-side
routes, so a hard refresh on `/conversations/abc123` doesn't 404. This was
tracked as its own ticket against loomux-server rather than LOOM-24
(scoped to the client repo), exactly as flagged when this design was
originally written.

## Error handling

- Network/API failure on any call → surfaced as an inline banner/toast
  carrying the server's `error` message verbatim (mirrors
  `core-design.md`'s "never silently drop" stance for routing failures,
  extended to every client-side call) — never swallowed.
- SSE stream drop → `fetchEventSource`'s built-in reconnect; a
  "reconnecting…" indicator shown if disconnected for more than 5 seconds.
- API version mismatch (`/version` reports something this build wasn't
  built against) → persistent banner, app remains otherwise usable — a
  soft warning, not a hard block, since `v1` is the only version that
  exists today and a false negative here (blocking a client that's
  actually fine) would be worse than a stale-but-functional one.
- `404` on `GET /conversations/{id}` (unknown conversation) → the
  composer still works (a first message to a fresh `conversation_id`
  always 404s on history before it exists) — this is expected, not an
  error state to surface to the user.

## Testing strategy

Vitest + React Testing Library for components, hooks, and the auth layer;
the API client tested against a mocked `fetch` rather than a real server.
No end-to-end tooling (Playwright, etc.) for v1 — YAGNI for a single-user
pre-alpha tool with one active developer; revisit once LOOM-24 build-out is
further along and the surface is stable enough to be worth the maintenance
cost of E2E tests.

## Deferred (separate future work, several already filed)

- Android, iOS clients (LOOM-26, LOOM-30)
- SSO/OAuth login
- GitHub-provisioning UI (LOOM-28, LOOM-29)
- Offline support / PWA install
