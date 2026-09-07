# Loomux — Message/Turn-Level Logging (LOOM-31)

## Overview

The registry has no stored "conversation" entity and no per-turn chat log —
only `Task` rows (workspace, status, timestamps), per `core-design.md` §2.
LOOM-18 built `GET /conversations/{id}` against exactly that: task
lifecycle history, not chat text. That was a deliberate, correct scope call
at the time (see `api/README.md`'s own note), but it left a documented gap
once a real client exists: `docs/design/web-client-design.md` ("Known API
gap: no persisted message transcript") has the web client faking a
transcript from local browser-tab state, since the server has nothing to
serve.

This spec adds message/turn-level storage — a small, additive extension of
the existing registry/storage pattern (§2, §8), wired into the existing
`GET /conversations/{id}` endpoint (LOOM-18) rather than a new API surface.

## Scope

**In scope:**
- A `messages` table + `registry.Message` domain type, following the
  swappable-backend `Store` interface pattern (LOOM-3)
- Persisting one turn (a `Router.Dispatch` call) as a `user` + `assistant`
  message pair
- Exposing stored messages via the existing `GET /conversations/{id}`
  response

**Out of scope:**
- Streaming/partial message content — `Router.Dispatch` only ever produces
  one condensed reply string per turn (capture-pane → LLM relay → `reply`);
  there is no finer-grained data in the pipeline to log without changing
  completion/relay itself
- Retention/purge tooling (see "Retention," below)
- A dedicated `/conversations/{id}/messages` endpoint — the existing
  conversation-detail endpoint is extended instead, since it already scopes
  by `conversation_id` and the ticket asks to wire into it directly

## What a "turn" is

One turn = one `Router.Dispatch(ctx, conversationID, message)` call,
regardless of which branch it takes (`answer_directly`, `use_workspace`, or
`provision_workspace` followed by dispatch to the agent). Each turn is
stored as **two `messages` rows**, `role=user` and `role=assistant`, rather
than one wide row with both texts — this matches the shape a chat-transcript
API/UI actually wants (one row per bubble) and stays forward-compatible if
a future role (`system`, `tool`) is ever needed. "Per-raw-message" (logging
every intermediate agent utterance) isn't achievable today: the relay model
condenses a whole captured pane into a single reply string, so there's
nothing finer to log.

## Data model

```go
type MessageRole string

const (
    MessageRoleUser      MessageRole = "user"
    MessageRoleAssistant MessageRole = "assistant"
)

// Message is one entry in a conversation's chat transcript. Append-only —
// unlike Workspace.RollingSummary, nothing ever updates a Message in
// place, so there is no UpdateMessage method.
type Message struct {
    ID             string
    ConversationID string      // bare string, same non-FK convention as Task.ConversationID —
                                // a conversation isn't a stored entity (§2)
    TaskID         string      // empty when the turn never touched a task (an
                                // answer_directly turn)
    Role           MessageRole
    Content        string
    CreatedAt      time.Time
}
```

`Store` gains two methods, matching the existing narrow-surface style
(`CreateTask`/`ListTasksByWorkspace`, etc.):

```go
CreateMessage(ctx context.Context, m *Message) error
ListMessagesByConversation(ctx context.Context, conversationID string) ([]*Message, error)
```

No `UpdateMessage`/`DeleteMessage` — nothing in this design ever mutates or
removes a message once written (see Retention).

### Migration

`registry/sqlite/migrations/00007_create_messages.sql`:

```sql
CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    task_id         TEXT REFERENCES tasks (id) ON DELETE SET NULL,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

CREATE INDEX idx_messages_conversation_id ON messages (conversation_id);
CREATE INDEX idx_messages_task_id ON messages (task_id);
```

`task_id` is `ON DELETE SET NULL`, not `RESTRICT` like every other FK in
this schema (`workspaces.target_id`, `tasks.workspace_id`,
`credentials.workspace_id`) — those all protect referential integrity for
rows that are actively used elsewhere; a message's task reference is just a
correlation convenience. The message log is meant to be the durable
transcript, so if a task row is ever administratively deleted, its messages
should survive rather than either blocking the delete or disappearing with
it. (No code path deletes tasks in production today — `DeleteTask` exists
on `Store` but is only exercised by the conformance suite — so this is a
forward-looking choice, not a fix for an active problem.)

## Where it's written

`Router.Dispatch` and `Router.dispatchToAgent` are the two places
`message`/`reply` text already exist together — both rows for a turn are
written there, **only once a reply is actually available**:

- `ActionAnswerDirectly` branch: write `[user, assistant]` (empty
  `TaskID`) immediately before returning `decision.DirectAnswer`.
- `dispatchToAgent`: write `[user, assistant]` (`TaskID = task.ID`)
  immediately before returning `result.Reply`, after the task has been
  resolved/launched so `TaskID` is known.

Deliberately **not** written on any error path (routing failure, launch
failure, relay failure, etc.) — this keeps the invariant "every stored turn
has both a user message and an assistant reply" always true, so the API
never has to represent a half-written turn. This is a real tradeoff:
a `Dispatch` call that starts routing but errors before producing a reply
leaves no trace in the transcript, even though the caller's HTTP request
technically happened. That's acceptable because the caller already gets
the failure synchronously via `POST /dispatch`'s error response (design
spec's "never silently drop" concern is about the reply reaching the user,
which it does) — nothing is silently lost from the human's perspective,
only from the stored-transcript's.

The shell-kind provisioning task inside `provisionWorkspace` is not logged
as a chat message — it's workspace-setup plumbing, not chat content. Only
the eventual `dispatchToAgent` call that follows it produces a logged turn.

## API

`GET /conversations/{id}` (`api/server.go`) gets an additive `messages`
field alongside the existing `tasks` field:

```go
type messageSummary struct {
    ID        string    `json:"id"`
    Role      string    `json:"role"`
    Content   string    `json:"content"`
    TaskID    string    `json:"task_id,omitempty"`
    CreatedAt time.Time `json:"created_at"`
}

type getConversationResponse struct {
    ConversationID string             `json:"conversation_id"`
    Tasks          []conversationTask `json:"tasks"`
    Messages       []messageSummary   `json:"messages"`
}
```

Oldest first, matching `tasks`' existing order. Backed by a new narrow
`MessageLister` seam (`ListMessagesByConversation`), mirroring `TaskLister`
— satisfied structurally by `*app.App.Store()`, no new dependency wiring
beyond `NewServer`'s parameter list.

**404 condition widens**: currently a conversation with zero tasks 404s.
That's now wrong — a conversation that only ever received
`answer_directly` replies has real message rows but zero tasks, and would
incorrectly 404 even though it has content to serve. The check becomes "no
tasks **and** no messages 404s, otherwise 200" — a small behavior change to
an existing endpoint, not pure addition, called out explicitly since it's
the one place this design touches existing behavior rather than only
adding new fields.

`GET /conversations` (the list endpoint) is unaffected — it already groups
by `Task.ConversationID` for its "most-recently-updated" summary and stays
that way; widening it to also surface answer-direct-only conversations in
the list view is a separate, un-asked-for scope expansion and is not done
here.

## Retention

**No automatic purge for v1.** Matches this codebase's existing precedent:
task rows have no reaper/TTL, and session rows are only opportunistically
cleaned up on next use (`api/README.md`: "harmless bloat for a single-user
table"). Messages are kept indefinitely; nothing in this design adds a
background sweep, size cap, or manual admin-delete path. If storage growth
ever becomes a real concern for a single-user pre-alpha tool, that's a
separate future ticket, not speculative machinery built in now.

## Testing strategy

- `registry/storetest`: new conformance cases (`Message` CRUD-minus-D/U,
  `MessageNotFoundConversation` returning an empty list rather than an
  error, `MessageListByConversationOrdering`,
  `MessageTaskDeletionSetsNull`), run against the shared suite so both the
  current sqlite backend and any future backend are held to the same
  contract, per the design spec's Testing Strategy section.
- `router`: `routertest`-backed tests asserting `Dispatch` writes exactly
  the expected `[user, assistant]` pair for both the `answer_directly` path
  and the agent-dispatch path (including the continuation case — a second
  turn in the same task appends two more rows rather than replacing
  anything), and that a failed dispatch writes nothing.
- `api`: `server_test.go` additions for `messages` in the
  `GET /conversations/{id}` response (populated + ordering), and the
  widened 404 behavior (message-only conversation now returns 200);
  `integration_test.go` extended so the real end-to-end stack (real
  sqlite, fake LLM vendor) proves a real `/dispatch` call's turn shows up
  in a subsequent `/conversations/{id}` fetch.

## Deferred (not in this pass)

- Retention/purge policy, if storage growth ever becomes a real concern
- A dedicated `/conversations/{id}/messages` endpoint (only the existing
  conversation-detail endpoint is extended)
- Persisting partial/failed turns
- Surfacing answer-direct-only conversations in `GET /conversations`'s
  list view
