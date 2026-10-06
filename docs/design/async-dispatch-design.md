# Async dispatch jobs (LOOM-80)

**Status:** approved 2026-10-04 (with the rollout change below) · **Ticket:** LOOM-80 (Vikunja Loomux #1199), epic LOOM-73 Phase 1
**Source:** command-center `reports/2026-10-02-loomux-dispatch-failure-modes.md`, finding A1, proposal "Async dispatch jobs"

## Problem

`POST /api/v1/dispatch` runs the whole turn inside the request (`api/server.go` `handleDispatch` →
`Dispatch(r.Context(), …)`). A closed tab, a sleeping phone or a proxy timeout cancels that context.
Since LOOM-77 the task no longer stays `running` forever (the deferred guard in
`router.dispatchToAgent` fails it as `wait_failed`), but the turn is still thrown away: the agent
keeps working in its pane, nobody waits for it, and no reply is ever delivered or stored.

## Prerequisite: bounded waits

The ticket depends on "Bounded waits for every turn". That is **LOOM-76, merged** (`7d69b6c`, PR #125):
`completion` wraps every wait in `MaxTurnDuration` (default 1h) and, for TierMarker, `NoProgressTimeout`
(default 10m), returning `orchestrator.TurnTimeoutError`; the router fails the task with class
`timeout` and keeps the pane. Routing/relay LLM calls carry their own per-call timeouts
(`llmrouter`). So a job running on a context with no request deadline is still bounded. No need to
do it first or fold it in. The job adds one backstop ceiling (below) for anything outside a wait.

## Shape

```
POST /dispatch ──► dispatch.Service.Submit ──(one tx)──► dispatches row (queued)
      │                                      └────────► messages row (user, dispatch_id)
      │ 202 {dispatch_id, conversation_id, status}
      ▼
   goroutine on server-owned ctx ──► Router.Dispatch(…, WithDispatchID, WithUserMessageLogged)
      │                                   └── assistant message (dispatch_id, task_id)
      ▼
   dispatches row → succeeded{reply} | failed{error, error_class}
      ▼
   SSE dispatch_update (polled from the row, like task_update today)
```

### New package `dispatch/`

One unit with one job: own dispatch jobs' lifetime. Depends on a narrow store interface and a
`Dispatcher` func (the router), not on `api`.

- `Service.Submit(ctx, req) (*registry.Dispatch, created bool, err)`: validates, applies idempotency,
  persists job + user message in one transaction, starts the runner goroutine.
- `Service.Wait(ctx, id) (*registry.Dispatch, error)`: blocks until terminal or `ctx` done (used by
  `?wait=true`; the caller giving up never affects the job).
- `Service.Get`, `Service.ListByConversation`.
- `Service.Shutdown(ctx)`: stop accepting, drain, mark the rest (see Shutdown).
- `Service.Recover(ctx)` at startup (LOOM-82): a `queued` row left by a previous process never
  started, so it is run now. A `running` one is offered to the resumer (`WithResumer`, the router's
  `ResumeDispatch`): when its conversation has an agent task still running, the job carries on by
  waiting on that turn, then relays and records the reply, so a restart mid-turn still delivers it.
  If that session is gone, the task fails (`session_lost`) and so does the job. Anything else is
  marked `interrupted` with reason `server_restart`. Then `Router.ReconcileTasks` (background, 2 min
  bound) fails every other running task whose session is gone, and finishes a command that exited
  meanwhile.

Runner context: derived from a service-owned root context created at
`app.Build`, cancelled only by `Shutdown`, plus a backstop `context.WithTimeout` of
`LOOMUX_DISPATCH_MAX_DURATION` (default `2h`: provisioning wait + one max-length turn + relay).

### Table `dispatches` (migration `00011_create_dispatches.sql`)

| column | type | notes |
|---|---|---|
| id | TEXT PK | uuid |
| conversation_id | TEXT NOT NULL | indexed |
| message | TEXT NOT NULL | the user's text, kept so the job can run (and LOOM-82 can retry) |
| workspace_hint | TEXT NOT NULL DEFAULT '' | |
| idempotency_key | TEXT NULL | `UNIQUE` where not null |
| request_hash | TEXT NOT NULL | sha256 of (conversation_id, message, workspace_hint), for key-reuse checks |
| status | TEXT NOT NULL | see state machine |
| reply | TEXT NOT NULL DEFAULT '' | set on `succeeded` |
| error | TEXT NOT NULL DEFAULT '' | set on `failed`/`interrupted` |
| error_class | TEXT NOT NULL DEFAULT '' | reuses `registry.ErrorClass` (+ `interrupted`) |
| created_at, started_at, finished_at | TIMESTAMP | started/finished nullable |

`messages` gains a nullable `dispatch_id` column so a client can tie a user bubble to its job (the
retry/error card in LOOM-81 hangs off it).

### State machine

```
queued ──► running ──► succeeded
   │          ├──────► failed        (Router.Dispatch returned an error)
   └──────────┴──────► interrupted   (shutdown drain expired, or found on startup)
```

Terminal: `succeeded`, `failed`, `interrupted`. Transitions are compare-and-set on `status`
(`UPDATE … WHERE id=? AND status=?`), so a late runner can't overwrite an `interrupted` mark.

### Message persistence

The user message is written at submit time, in the same transaction as the job, so reopening the
conversation always shows it, even while the turn is running or after it fails. The router gets
two `DispatchOption`s: `WithDispatchID(id)` (stamped on the messages it writes) and
`WithUserMessageLogged()` (its `logTurn` then writes only the assistant message). The in-process
CLI path (`loomuxd -message`) doesn't go through jobs and keeps today's behaviour.

A failed job records its error on the job row only. Writing an error message into the transcript is
LOOM-95's job; this design leaves room for it (it can key off `dispatch_id`).

### Idempotency

`Idempotency-Key` header (optional, ≤ 255 chars).

- Unseen key: create the job, store the key.
- Seen key, same `request_hash`: return the existing job (202, or under `?wait=true` its result),
  never a second dispatch. Duplicate detection is the unique index, so two concurrent identical
  POSTs race safely: the loser's insert fails, it re-reads and returns the winner's job.
- Seen key, different `request_hash`: `422 idempotency_key_reused`.

Keys live as long as the row. No expiry (single-user deployment, rows are small); revisit with
retention if it matters.

### One active job per conversation

Without a blocking request, double submits get easier. A second `POST` for a conversation that
already has a `queued`/`running` job gets `409 {error, dispatch_id}` naming the active one. This is
deliberately the minimal guard: LOOM-83 owns serialization and may replace the 409 with a queue.

## API

### `POST /api/v1/dispatch`

Request: `{conversation_id?, message, workspace_hint?}`, optional `Idempotency-Key` header.
`conversation_id` becomes optional: if empty the server mints one and returns it.

Every request creates (or, by idempotency key, finds) a job. The mode only decides whether the
response waits for it:

- **async**, asked for with `Prefer: respond-async` (RFC 7240) or `?async=true` → `202 Accepted`,
  `Location: /api/v1/dispatches/{id}`, `Preference-Applied: respond-async` when the header was used,
  body `{dispatch_id, conversation_id, status}`.
- **blocking**, asked for with `?wait=true`, or the default (see Rollout) → blocks until the job is
  terminal: `200 {reply, dispatch_id, conversation_id}` on success, `500 {error, error_class,
  dispatch_id, conversation_id}` on failure (today's status, `reply`/`error` keys unchanged). If the
  client disconnects the job carries on.
- `?wait=true` together with an async request → `400`.
- `400` malformed / empty message, `409 {error, dispatch_id}` conversation busy,
  `422` key reused, `503` while shutting down.

### `GET /api/v1/dispatches/{id}`

`{dispatch_id, conversation_id, status, reply?, error?, error_class?, confirmation_id?, created_at, started_at?, finished_at?}`; `404` unknown.

### Cancelling a turn (LOOM-99)

`POST /api/v1/dispatches/{id}/cancel` → `202 {dispatch_id}`: the job's context is cancelled with
cause `orchestrator.ErrCancelled`. The router fails the turn's task with class `cancelled`, sends
the agent-type's interrupt keys (Escape for claude/codex) and keeps the pane to inspect. The job ends
`failed` with `error_class: cancelled`, which the stream reports. `404` unknown, `409` already ended.

`POST /api/v1/tasks/{id}/cancel` → `202 {task_id, dispatch_id?}`: a `running` task is the one its
conversation's running dispatch drives, so it is cancelled through that dispatch. Any other open task
(left awaiting input, stopped at a prompt, or left running by a restart) is failed and interrupted
directly, never through a dispatch driving another task of the conversation. `404` unknown, `409`
already ended or taken over by a person (nothing is typed into a pane they are driving).

A cancelled task is `failed` with class `cancelled`, not a status of its own. The tasks table's
status CHECK would need a table rebuild for a new status, and every "has it ended" check would need
to learn it. The class already says why it ended. A command task's or provisioning recipe's
pane is not interrupted, since each has its own time limit.

### `GET /api/v1/conversations/{id}`

Adds `dispatches: [...]` (same shape, oldest first) and `dispatch_id` on each message. A conversation
with only a queued job (no messages from the router yet) still resolves, because the user message
exists.

### SSE `GET /api/v1/conversations/{id}/stream`

New `event: dispatch_update`, `data: {dispatch_id, status, reply?, error?, error_class?, updated_at}`,
sent whenever a job of this conversation changes, from the same poll loop as `task_update` (which is
unchanged). On connect the stream sends the current state of any non-terminal job, so a client that
reconnects mid-turn picks up where it was. Stage events (routing/provisioning/…) stay LOOM-96.

## Shutdown

`main.go` order on SIGTERM:
1. `dispatch.Service.Shutdown(ctx)` with a drain budget (`LOOMUX_DISPATCH_DRAIN`, default `20s`;
   k8s grace is 30s): new submits get 503; in-flight jobs get the budget to finish.
2. Jobs still running then have their contexts cancelled with cause
   `orchestrator.ErrInterrupted`. Their rows are left `running`, so the next process's `Recover`
   resumes them (LOOM-82). Before LOOM-82 they were marked `interrupted`, which meant a deploy
   mid-turn could never be resumed.
3. Router: the LOOM-77 deferred guard in `dispatchToAgent` skips failing the task when
   `context.Cause(ctx)` is `ErrShutdown` (exposed as `orchestrator.ErrInterrupted` so router doesn't
   import `dispatch`). The task stays `running` with its pane alive, which is exactly what LOOM-82
   needs to re-attach after restart.
4. Then `httpServer.Shutdown` as today.

## Web impact (LOOM-81 picks this up)

- `api.dispatch` returns `{dispatch_id, conversation_id, status}`; stop expecting `reply`.
- Send an `Idempotency-Key` (uuid per submit, reused on network retry) so a retry never types twice.
- Render the in-flight turn from `dispatch_update` (and `task_update`), not from the pending
  request; on `succeeded` refetch history (or append `reply`); on `failed`/`interrupted` show an
  error card with Retry (a new POST with a new key, same text).
- On load, a non-terminal entry in `conversation.dispatches` means "turn in flight": show the card.
- Handle `409` (turn already running: attach to the returned `dispatch_id`) and `503`.
- The user message is persisted at once, so the optimistic `pendingUser` can be dropped after the
  202 + refetch.

**Rollout (decided 2026-10-04):** the pinned web (`deploy/web-ref` `e5276dd`) does `const {reply} =
await dispatch()`, so the server ships with **blocking as the default**: the web keeps working
unchanged and the server deploys on its own. Even in blocking mode the turn runs on the job's
server-owned context, so a closed tab no longer loses it; reopening the conversation shows the reply.
The default is one constant, `dispatchAsyncByDefault` in `api/dispatch.go`. Flipped to `true` once the
LOOM-81 web (which asks for async explicitly anyway) shipped and was pinned (`deploy/web-ref` `d14a402`):
a client that wants the reply in the response now asks with `?wait=true`.

## Testing

TDD, `go test -race` on `dispatch`, `registry/sqlite`, `router`, `api`, `app`, `cmd/loomuxd`.

- `registry/sqlite`: migration, create/get/list, CAS transitions, unique key, `messages.dispatch_id`.
- `dispatch`: submit→running→succeeded/failed with a fake dispatcher; submit's ctx cancelled right
  after returning doesn't affect the job; idempotent duplicate (sequential and concurrent) yields
  one dispatcher call; key reuse with different body errors; busy conversation; `Wait` returns on
  terminal and on its own ctx; shutdown drains a quick job, interrupts a slow one with
  `ErrShutdown` cause; `Recover` marks leftovers.
- `router`: `WithUserMessageLogged` writes only the assistant message; `dispatch_id` stamped;
  shutdown cause leaves the task `running`.
- `api`: 202 shape + Location, `?wait=true` 200/500, client disconnect mid-wait then
  `GET /dispatches/{id}` and conversation history show the reply (acceptance #1), same key twice
  → one dispatch (acceptance #2), 409/422/503, SSE `dispatch_update` incl. initial state on connect.
- docs: `api/README.md`, `docs/design/core-design.md` client-API section, `web-client-design.md`.
