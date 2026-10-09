# Retention defaults: proposal (LOOM-175 item 8)

**Status:** implemented (LOOM-193, 2026-10-09) for sessions, dispatches
and confirmations, with the defaults below, by a retention sweep of its
own (`internal/retention`, hourly) rather than the reaper: a session's
expiry depends on the API's lifetime settings, so the server starts the
sweep once it has read them. Messages are kept and have no setting. A
deleted dispatch's messages stay, their `dispatch_id` set to NULL. The
absolute session lifetime (`LOOMUX_SESSION_MAX_AGE`, 90 days) shipped
with LOOM-175.

The database grows with use, and some of what it keeps is sensitive
(what was asked, what agents answered, which commands ran). Two tables
already have a retention setting, run by the reaper's sweep:
`LOOMUX_TURN_RETENTION` (per-turn transcripts, 30 days) and
`LOOMUX_EVENT_RETENTION` (the dispatch audit trail, 90 days). This
proposes defaults for four more, in the same shape: an env var holding a
duration, `0` keeping rows forever, applied by the same sweep.

## Today

| Table | Deleted when | Grows with |
|---|---|---|
| `sessions` | logout or revoke; an expired session's row only when its token is next presented (`requireAuth`'s opportunistic cleanup) | each login |
| `dispatches` | never | each turn (the message and the reply, in full) |
| `messages` | never (`docs/design/message-logging-design.md`, "Retention") | each turn, two rows |
| `confirmations` | never (pending ones become `expired` at restart or timeout) | each offer |

The idempotency key of a dispatch lives as long as its row
(`docs/design/async-dispatch-design.md`).

## Proposal

| Setting | Default | Deletes |
|---|---|---|
| `LOOMUX_SESSION_RETENTION` | 7 days | session rows past their sliding window or absolute lifetime for this long. They can't authenticate anyway; this only removes the rows (and their token hashes). |
| `LOOMUX_DISPATCH_RETENTION` | 90 days | finished dispatches (`succeeded`, `failed`, `interrupted`) older than this. Never a queued or running one. Its idempotency key goes with it: a client retrying a 90-day-old key would start a new turn, which is fine. |
| `LOOMUX_MESSAGE_RETENTION` | `0` (keep) | messages older than this. |
| `LOOMUX_CONFIRMATION_RETENTION` | 30 days | resolved confirmations (`approved`, `declined`, `expired`) older than this, by `resolved_at`. Never a pending one. |

Why these numbers:
- **Sessions, 7 days.** An expired row is dead weight: `GET /sessions`
  already hides it and its token is refused. The short grace period only
  helps someone reading the database after an incident ("when did that
  device last sign in?").
- **Dispatches, 90 days**, the same as the audit trail they're the subject
  of. A dispatch row duplicates its turn's two messages, so deleting it
  loses no history.
- **Messages, kept.** They are the conversation history the web client
  shows. Deleting them by age makes old conversations look truncated, and
  a conversation is the user's own record. Offer the setting, default it
  off, and revisit if storage becomes a real problem. A per-conversation
  "delete" is likely the better tool.
- **Confirmations, 30 days.** A resolved offer matters while its
  conversation is active; the audit trail keeps the record of it longer
  (an `offer` and its answer are dispatch events).

## Interactions to decide

- **A conversation whose dispatches and confirmations are gone** still
  shows its messages; `GET /conversations/{id}` already copes with empty
  lists. Nothing points from a message to a deleted dispatch except
  `messages.dispatch_id`, which would dangle. Set it to NULL on delete, or
  accept it (the web client doesn't follow it).
- **Backups** (`docs/deploy/backup-restore.md`) keep whatever was in the
  database when they were taken. Retention doesn't reach them.
- **Order of rollout.** Sessions first (no user-visible effect), then
  confirmations and dispatches. Messages only if asked for.

## Implementation sketch

One `Delete…Before(ctx, cutoff)` per table in `registry.Store`, like
`DeleteTaskTurnsBefore`. A reaper option each (`WithSessionRetention`, …).
App config parses the env vars like the existing two. Each gets a
storetest conformance case: rows older than the cutoff go, newer and
active ones stay.
