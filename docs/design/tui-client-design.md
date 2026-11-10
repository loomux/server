# Loomux — Terminal Client Design (LOOM-261)

**Status:** draft for review, 2026-11-05; every open question decided by the user the same day (see "Decided") · **Ticket:** LOOM-261 (Vikunja Loomux id 2359, GitHub #488)
**Blocks:** LOOM-235, the cutover of command-center into a Loomux-managed pod (LOOM-225)

## Overview

A terminal client, `loomux`, for someone who already lives in a terminal. It
does more than chat. It uses the API for everything the web does day to day
(conversations, the live stream, answering a prompt, cancelling). It can also
attach to a live agent's own TUI (Claude Code, Codex) in its tmux pane, work
there natively, detach, and land back in the list.

Two properties shape the whole design:

1. **The server always knows who is driving.** An attach is a *takeover*
   recorded on the server with a lease, so the web and the apps show "driven
   from the terminal on `laptop`". When the terminal detaches, the hold is
   released. When it crashes, the hold expires.
2. **No state lives only in the terminal.** Conversations, turns, prompts,
   offers and holds are all server state. Work started in the terminal can be
   picked up from the web or the app, and the other way round.

The motivating case: today the user drives command-center through Claude
Code's TUI inside tmux on their own machine. After LOOM-235 that session
runs in a pod Loomux made. This client keeps that way of working (attach,
type, detach), adds the attention-first overview the web has, and keeps the
server and the other clients in step.

Like `web-client-design.md`, this is a client design. Unlike the web design,
it can't treat the API as a fixed contract. Takeover/release has never been
exposed, and token issuing assumes a password form. Both need server work,
specified here.

Every API path in this document is under `/api/v1` and is written in full.
The routes that don't exist on `main` yet are proposals:
`POST /api/v1/tasks/{id}/takeover`, `…/takeover/heartbeat` and
`…/release` (S3), `POST /api/v1/sessions` (S7), `GET /api/v1/stream`
(L1) and `GET /api/v1/tasks/{id}/terminal` (L4a, only if wanted). They're
listed under "Changes outside the client" and "Build plan".

## Goals, non-goals, anti-goals

**Goals**
- An attention-first list of conversations: what needs the user first, then
  what's running, then what's done (the same ranking as the web's
  `src/lib/conversations.ts`).
- Chat with a live stream: send, watch the turn, read the reply, cancel.
- Answer a blocked agent prompt (needs-attention) and approve or deny an offer
  without leaving the keyboard.
- Attach to a live agent's native TUI and detach back, always through
  takeover/release with a lease.
- Hand work between the terminal, web and app in both directions at any time.
- Log in through the browser (forward auth and MFA included). The token goes
  to the configured token store (the OS keyring by default, an opt-in
  `0600` file where there's none) and can be revoked from the web or the
  terminal.
- One static binary for Linux and macOS.

**Non-goals (v1)**
- Administration: targets, plugins, machines, SSH keys, credentials and
  router settings stay in the web. The terminal may link to the web page.
- Windows. It may build, but attach and the keyring aren't tested there.
- Desktop notifications. The existing "needs you" notifications already link
  to the web.
- Server-proxied attach over a WebSocket (decided 5: later tickets L4a–c).
- A two-pane layout for wide terminals (decided 2: a later ticket, L5, only
  if it's missed).

**Anti-goals: things this client must never do**
- Prompt for, store or send the Loomux password, or work around the forward
  auth proxy.
- Attach without a hold. The client never runs a raw `tmux attach` behind the
  server's back. A person can still do that by hand outside Loomux, as today.
- Turn the pane into chat: no scraping or re-rendering an agent's screen as
  messages. The agent's TUI is the interface while attached. What it
  finishes is relayed afterwards by the server's late-output pass.
- Make routing decisions or keep its own copy of conversation state. Like the
  web, it renders what the server says.
- Bypass a gate the server enforces: target policy, confirmations, the
  one-dispatch-per-conversation rule, takeover.

## What main has today (read at `f5e8a68`)

Findings that shape the server work below:

- **Takeover/release exist but nothing calls them.** `orchestrator/takeover.go`
  flips a task to `human_takeover` and back to `awaiting_input`. No route,
  CLI or other caller reaches either function. The `loomux takeover` command in
  `core-design.md` §4 was never built. With `human_takeover` set, the
  following refuse: `SendMessage` (`ErrHumanTakeover`), task cancel (`409`),
  workspace delete (`409`) and the attention answer. The reaper skips the
  task. So the server-side gates are in place. The missing parts are the
  routes and any idea of *who* holds the task and for how long.
- **A turn's wait overwrites a takeover.** `Orchestrator.WaitForCompletion`
  reads the task, waits, then writes the whole row back as `awaiting_input`
  (`registry/sqlite` `UpdateTask` is an unconditional full-row update). A
  takeover during a running turn would be undone when the turn ends, while
  the person is still typing. `core-design.md` accepted "last writer wins" for
  v1 because nothing could take over. Once something can, this must change
  (ticket S1).
- **Answering a prompt is a chat message.** A `needs_attention` task carries
  `attention` (`kind`, `title`, `detail`, `question`, `options[{label,
  description}]`, `selected`, `resets`). The conversation's next message is
  read deterministically: yes/approve, no/deny, an option number, or text
  (`core-design.md`, "Agent stopped at a prompt"). An offer is answered by
  `POST /api/v1/dispatch` with `confirmation_id` and "yes" or "no" (LOOM-123). There
  are no dedicated approve/deny routes and none are needed.
- **Attach info gives shell strings.** `GET /api/v1/tasks/{id}/attach-info` returns
  `attach_commands: [{via, command}]`: the plugin's (`kubectl … exec -it …`,
  `docker [-H …] exec -it …`) and the plain `ssh` form for every remote
  target. These are strings meant for a person to copy. A program has to
  split them, so an `argv` form is proposed (S8).
- **There's no feed across conversations.** The SSE stream is per
  conversation. The web's list ranks `GET /api/v1/conversations` by the latest task's
  status. A conversation that ended on an offer awaiting a yes doesn't show
  as needing the user there (S6).
- **Sessions carry no label.** `GET /api/v1/sessions` shows `{id, created_at,
  last_used_at, current}`, so a "your devices" view can't say which row is the
  terminal (S7).
- `POST /api/v1/login` takes a password, and the deployment only exposes it through the
  browser flow behind the forward-auth proxy (next section).

## Main workflows

### 1. The attention-first list (home)

```
 loomux · test                                       ● stream ok   ?=help
 NEEDS YOU (3) ─────────────────────────────────────────────────────────
 ▲ needs attention  cc-hub       "Bash command: kubectl rollout …"   2m
 ? offer            infra-lab    "Create workspace on sc1?"         9m
 ⌨ held: laptop     cc-hub/2     "triage the inbox"                 1h
 RUNNING (1) ───────────────────────────────────────────────────────────
 ◐ running          loomux-web   "fix the flaky ConversationPage …" 30s
 DONE ──────────────────────────────────────────────────────────────────
 ✓ completed        notes        "summarise yesterday's PRs"        3h
 [n]ew  [enter] open  [a]ttach  [w]atch  [/] filter  [tab] needs-you/running/done/all
```

- Ranked like the web: `needs_attention`, `awaiting_input` and
  `human_takeover` first, then `running`, then `completed`/`failed`. Newest
  first within a group, `conversation_id` as tie-breaker. Conversations with
  a pending offer also rank first once S6 exposes it.
- Each row shows the status glyph, workspace, preview (the first message),
  the holder if held, and age.
- Live: v1 polls `GET /api/v1/conversations?limit=100` every 5 s and right after any
  action. The cross-conversation stream (L1) replaces polling later.
- `a` on a row attaches to that conversation's open agent task (workflow 4).
  `w` watches it read-only (a watch hold, see "Watching").

### 2. Chat with a live stream

Opening a conversation loads `GET /api/v1/conversations/{id}` (messages, tasks,
dispatches, confirmations) and opens `GET /api/v1/conversations/{id}/stream`.

- `message_added` → refetch the conversation. `dispatch_update` → update the
  in-flight turn card and, on `succeeded`, show the reply. `task_update` →
  update the task's status line.
- Sending is async `POST /api/v1/dispatch` with a fresh `Idempotency-Key` per
  submit, reused on retry (as the web does since LOOM-81). `409
  conversation_busy` puts the turn named by its `dispatch_id` in view rather
  than showing an error.
- `ctrl-c` (or `x`) on an in-flight turn → `POST /api/v1/dispatches/{id}/cancel`,
  after a confirm.
- Replies are markdown and are rendered as markdown (glamour). The composer
  is multi-line. `enter` sends and `alt-enter` adds a newline (decided 1).
- `n` from the list starts a conversation (empty `conversation_id`, the
  server mints one). Starting from a workspace sends that `workspace_hint`.
- `p` on a task shows its transcript (`GET /api/v1/tasks/{id}/transcript`, paged):
  each turn's pane at its end, without attaching.

### 3. Answering a blocked prompt or an offer

A `needs_attention` task renders as a card:

```
 ▲ Claude Code is asking  (permission)
   Bash command
   kubectl rollout restart deploy/api -n demo
   Do you want to proceed?
   1) Yes   2) Yes, and don't ask again for kubectl   3) No
   [1-9] choose  [y] yes  [n] no  [r] reply with text  [a] attach instead
```

Each key sends the message the server already reads deterministically: the
option's number, "yes", "no", or the typed text. The terminal never chooses
keys for the agent itself. Only `permission`, `question` and `trust`
prompts become such a card.

A sign-in screen, a usage limit or an API error never reaches
`needs_attention`. `router/attention.go` fails the task with
`error_class` `login_required`, `agent_rate_limited` or `agent_api_error`,
keeps the pane, and `failTask` clears `attention`. The terminal renders a
**failed-task card** keyed on that `error_class`. It shows
`failure_reason` (which carries the reset time for a usage limit) and offers
attach as its action, since signing in or waiting out the limit is the
user's to do in the pane. Takeover of such a failed task is described under
"API".

An offer card (pending confirmation) has `y`/`n`, which send "yes"/"no" with
its `confirmation_id`. A stale offer is refused by the server and shown as
such.

### 4. Attach to a live agent and detach

```
 list/chat ──a──► POST /api/v1/tasks/{id}/takeover ──► hold + attach info
                         │
                         ▼
        suspend the TUI, run the attach command on this terminal
        (heartbeat goroutine renews the hold every 15 s)
                         │   user works in Claude Code / Codex natively
                         ▼
        attach command exits (detach, ssh drop, pane gone)
                         │
                         ▼
        POST /api/v1/tasks/{id}/release {hold_id} ──► resume the TUI, refresh
```

Details are under "Server awareness" and "Attach mechanics".

### 5. Handing work between clients

- **Terminal → web/app.** Everything is already on the server. Someone who
  walks away while attached sees "driven from the terminal on `laptop`" on
  the phone and can **Take back**, which releases the hold and detaches the
  terminal's client (S4). Pressing `o` opens the conversation in the browser
  (`<server>/conversations/<id>`).
- **Web/app → terminal.** The conversation is in the list. `loomux open <id>`
  jumps straight to it, and the web can show that command as copyable text
  next to its attach info (W2). Attaching from the terminal while the web
  shows the conversation updates the web live (S6's `task_hold` event).
- **Terminal → terminal.** A second terminal with a different session that
  tries to attach sees `409 task_held` naming the holder and can force the
  takeover (with a confirm). The first terminal's client is detached. The
  same session re-attaching (the TUI restarted after a crash) gets its own
  hold back.

### 6. Logging in and out

`loomux login [--profile test]` and `loomux logout`, described under "Auth".

## Server awareness: holds (lease + heartbeat)

### Model

A **hold** is a lease on one task by one session:

| field | |
|---|---|
| `id` | uuid |
| `task_id` | the held task |
| `session_id` | the API session that took it (from `requireAuth`) |
| `client_kind`, `client_label` | `tui` + `laptop`, copied from the session (S7) or sent with the takeover |
| `mode` | `drive` or `watch` (both in v1, see "Watching" below) |
| `prev_status` | the task's status when taken, for display and audit |
| `ttl_seconds` | the lease's length, 15–300, default 45. Stored (added while building S2), because a renewal pushes `expires_at` out by it and it can't be read back from the other times once the restart grace has moved `expires_at` |
| `acquired_at`, `renewed_at`, `expires_at` | |
| `ended_at`, `ended_reason` | `released`, `expired`, `taken_back`, `taken_over`, `session_revoked`, `task_ended` |

New table `task_holds` (migration `00036`), one active `drive` hold per task
(a partial unique index on `task_id` where `ended_at IS NULL AND mode =
'drive'`). Ended rows are kept for the audit trail and swept by retention
like confirmations.

The hold and the task's status are written together (S2's
`AcquireDriveHold` and `ReleaseTaskHold`, each one transaction). Written
separately, a release racing a takeover could leave a held task in
`awaiting_input`, where automation may type into a pane a person is
driving.

The hold is the source of truth for *who*. The task's status
`human_takeover` remains the source of truth for *whether automation may
type*, so every existing gate keeps working unchanged.

### Watching (decided 12: in v1)

A **watch** hold is a read-only attach that still tells the server who is
looking. It's needed for the cutover: the user can look at the hub's agent
without pausing it.

- `POST /api/v1/tasks/{id}/takeover` with `mode: "watch"` (S10). The task's
  status doesn't change and automation keeps typing. Any number of watch
  holds may exist per task, alongside at most one drive hold. A held or
  failed task with a kept pane can be watched too, whatever the drive rules
  say. Heartbeat, release, expiry and revocation work as for drive holds.
- A server that can't watch never hands out a drive hold for a watch
  request, and the client never runs a command for an unconfirmed watch
  (see "Versions and compatibility").
- Expiry of a watch hold detaches nobody. A read-only client can't collide
  with automation, and `detach-client -s` would detach everyone in the
  session.
- The response's attach commands are read-only (`tmux … attach -r -t
  <session>`, S11). The ssh and local forms are built by the server. A
  plugin is asked with `read_only: true` in `AttachParams`, under a new
  capability flag. A plugin without the flag gets no watch command:
  the server answers `409 watch_unsupported` rather than hand out a
  writable command for a watch.
- Upgrading a watch to a drive is a new `mode: "drive"` takeover. The
  client detaches its read-only client first, then attaches writable.
- Other clients show watchers next to the holder ("👁 watched from
  `laptop`"). The `task_hold` event covers both modes, and list rows carry
  `watchers` (a count) beside `held_by`.

### API (additions within v1)

- `POST /api/v1/tasks/{id}/takeover` `{mode?, client_label?, ttl_seconds?,
  force?}` → `200 {task_id, hold, attach}`. `attach` is the attach-info
  body, so one round trip gets both. `ttl_seconds` is 15–300, default 45.
  `mode` is `drive` (the default) or `watch`. The rules below are for
  `drive`. `watch` is under "Watching".
  - The active drive hold is checked **before** the task's status:
    1. Held by this same session: the existing hold is renewed and returned.
       This is idempotent, and it's how a restarted TUI gets its own hold
       back on a task that is already `human_takeover`.
    2. Held by another session: `409 task_held` with the `hold`, unless
       `force`. `force` ends the other hold as `taken_over`, detaches its
       clients, and gives this session a new hold. The task stays
       `human_takeover` throughout.
    3. Not held: the status rules below apply.
  - An unheld task that is `running`, `awaiting_input` or `needs_attention`
    moves to `human_takeover` by compare-and-set. An unheld task already in
    `human_takeover` (only possible from before holds existed) is taken as
    if it were `awaiting_input`.
  - A `failed` task whose pane was kept for a person, with `error_class`
    `login_required`, `agent_rate_limited` or `agent_api_error`, can be held
    too. Its status stays `failed`, since nothing automated drives a failed
    task, and releasing it only ends the hold. This is so attaching to finish
    a sign-in still shows who is in the pane.
  - Any other ended task (`completed`, or failed for another reason) is
    `409` (`task_inactive`).
- `POST /api/v1/tasks/{id}/takeover/heartbeat` `{hold_id}` → `200 {hold}` with
  `expires_at` pushed out by the TTL. An ended hold is `410 hold_ended` with
  `ended_reason`. Only the session that holds it may renew it (`403`
  otherwise, added while building S3): a heartbeat says the holder is
  alive, which nobody else can say for it.
- `POST /api/v1/tasks/{id}/release` `{hold_id?}` → `200 {task_id, status,
  hold?}` (`status` is the task's afterwards):
  - With `hold_id`, ends that hold as `released`. Idempotent: an
    already-ended hold answers `200` with its reason.
  - Without it, it's **Take back**: ends whatever drive hold is active as
    `taken_back` and detaches its clients. Single user, so any session may.
  - Either way a `human_takeover` task goes to `awaiting_input` (as
    `Orchestrator.Release` does today) and its stale `attention` is cleared.
    The hold's end and that status change are one write (S2/S3), and they
    happen only when no drive hold is left on the task: a release with a
    stale `hold_id`, after another session forced a takeover, never hands
    the task back from under the new holder.
    Anything the agent finished while driven is relayed by the existing
    late-output pass, whose own comment already covers "a task handed back
    from a human takeover" (`router/lateoutput.go`).
- Exposure (S6): `hold` (active or null) on each task of
  `GET /api/v1/conversations/{id}` and on attach-info. On `GET /api/v1/conversations` rows:
  `held_by {client_kind, client_label, since}` and `pending_confirmation_id`.
  A new stream event `event: task_hold`,
  `data: {task_id, hold, ended_reason?}`, fires on every acquire, end and
  steal (not on renewals).
- A message sent to a held conversation fails today with `error_class:
  internal` (`router/errorclass.go` maps `ErrHumanTakeover` there), which a
  client can't tell from a real fault. S6 gives it its own class, `held`, and
  the reply text names the holder.

New error codes go in `api/README.md`'s table and the contract file is
regenerated (lines only added).

### Expiry, crashes and restarts

- **Heartbeat** every 15 s against a 45 s TTL, so two lost heartbeats are
  tolerated. A network error is retried every 5 s until the TTL. A `410`
  means the hold is gone and the server has detached (or is detaching) the
  client.
- **Expiry sweeper** (app goroutine, every 5 s): ends expired holds as
  `expired`. For a drive hold (decided 7) it then releases the task and
  **detaches the session's tmux clients** (`tmux -L <socket> detach-client -s <session>` through the
  target's executor, a new `DetachClients` on `TargetExecutor`). This fails
  closed: once the lease is gone, nobody keeps typing unannounced into a pane
  the router is about to drive. It also detaches any watcher attached by hand
  outside Loomux. That's acceptable for a path only a crash or long
  partition reaches. They can re-attach.
- **Crashed TUI.** Server side, the hold expires within 45 s and the client
  is detached. Client side, as belt and braces: on Linux the attach child gets
  `Pdeathsig: SIGHUP`, started from a goroutine locked to its OS thread (Go's
  `Pdeathsig` follows the thread that started the child, not the process).
  macOS has no equivalent, so there the server side is the guarantee.
- **Server restart or deploy.** Holds are rows, so they survive. On start,
  the sweeper first moves every active hold's `expires_at` to at least
  `start + TTL`, so a deploy longer than the TTL doesn't kick everyone out.
  A terminal still attached resumes heartbeats once the API answers.
- **Session revoked** (`POST /api/v1/logout`, `DELETE /api/v1/sessions/{id}`, expiry): its holds
  end as `session_revoked` and are released and detached, alongside the
  streams and waits LOOM-144/182 already end.
- **Task ends while held** (cancelled from elsewhere is already refused, but
  a pane can exit): the hold ends as `task_ended`.

### Taking over a running turn

Jumping into a busy agent is the main case for the hub. A `running` task may
be taken over:

- The turn's wait ends at once. Its dispatch finishes `failed` with a new
  error class `taken_over` (no interrupt keys are sent, unlike cancel), and
  the router's guard leaves the task `human_takeover` rather than failing it.
- Whatever the agent finishes while driven, or after release, reaches the
  conversation through late output, as above.
- This depends on S1: every status write that follows a wait or an external
  call must be a compare-and-set against the status it read
  (`WaitForCompletion`, the attention path, `failTask`, `Complete`), so the
  takeover can't be overwritten.

Decided (6): taking over a running turn is allowed, with the behaviour
above.

### What other clients show

- Web/app list and conversation: "⌨ driven from the terminal on `laptop` since
  14:02" with **Take back**. A message sent to a held conversation fails
  with error class `held` (S6), and the web shows the same banner for it
  (W2).
- Sessions page: the label and kind per row (`loomux TUI · laptop`), so
  revoking the right device is obvious.

## Auth

### Constraints

The deployments put a forward-auth proxy with MFA in front of Loomux. The
web, and with it `POST /api/v1/login`, are only reachable through that
proxy's browser flow. Requests to `/api/v1/` that carry a Loomux bearer token
skip the proxy, and `loomuxd` validates the token itself. So a terminal can't
reach `POST /api/v1/login`. It can't reach an unauthenticated "start device flow"
endpoint either, because a request without a bearer token is sent to the
proxy's login page. And it must never hold the password.

### The link flow (recommended)

The terminal makes its own token, and the browser approves only its hash:

1. `loomux login` generates a token `T` exactly as the server does (32 random
   bytes, base64url) and computes `H = hex(sha256(T))`, the value the server
   stores (`api/auth.go` `hashToken`). It shows a verification code
   `V = first 8 of base32(sha256("loomux-link-v1:" + H))`, formatted
   `ABCD-EFGH`.
2. It opens the browser at `<server>/link#h=<H>&label=<hostname>` (printed
   too, for a terminal on a remote machine). The fragment never reaches a
   proxy or server log, and `H` isn't a secret anyway.
3. The web's `/link` page (W1, a web route, not an API path) goes through the proxy's MFA and the web's own
   login as usual. It shows the label and `V`, says "approve only if your
   terminal is showing this code right now", and on **Approve** calls
   `POST /api/v1/sessions {token_hash: H, label, client_kind: "tui"}` with the
   web's own token (S7).
4. Meanwhile the terminal polls `GET /api/v1/sessions` with `Authorization:
   Bearer T` every 2 s for up to 10 minutes. The bearer header gets it past
   the proxy, and it gets `401` until the session exists, then `200`.
5. It stores `T` in the configured token store (the OS keyring, or the
   opt-in `0600` file, see "Storage and revocation") and is done. `T`
   itself never left the machine.

Why not a classic device-code flow (RFC 8628): its first call is
unauthenticated, which the proxy blocks. Exempting a path would put a new
unauthenticated, proxy-bypassing endpoint on the internet. In this flow every
call carries a bearer token, and the only new server surface is an
authenticated route.

Server side (S7):
- `sessions` gains `label`, `client_kind` (`web`, `tui`, `app`, `""`) and
  `origin` (`login` or `link`). `GET /api/v1/sessions` returns the first two.
- `POST /api/v1/sessions` accepts a 64-hex `token_hash`, a `label` (≤ 64
  printable characters) and a `client_kind`. `409` if the hash exists. It is
  **allowed only from a session whose origin is `login`**: a stolen terminal
  token can't mint itself successors that survive its revocation. A linked
  session has the same sliding TTL and 90-day max age as any other.
- `POST /api/v1/login` sets `origin: login` and `client_kind: web` (the web is its only
  caller).

Phishing note: as with any device flow, someone could send the user a link
carrying their own `H`. The code comparison and the page's wording are the
defence, and the sessions page shows every linked terminal for revocation.
For a single-user tool that's proportionate.

`GET /api/v1/version` is unauthenticated, but the proxy would still intercept
it without a bearer header. The terminal sends its stored token (or, during
login, the pending `T`) on every call, `GET /api/v1/version` included, and the handler
ignores it.

### Storage and revocation

The **token store** is chosen by `token_store` in the config: `keyring`
(the default) or `file` (decided 11).

- **Keyring** (`token_store = "keyring"`, the default). Service `loomux`,
  account `<profile>@<server URL>`, through `github.com/zalando/go-keyring`:
  Secret Service over D-Bus on Linux (GNOME Keyring, KWallet, KeePassXC),
  the Keychain on macOS. Pure Go, no cgo. If no keyring is available (a
  headless box, a container), `loomux login` refuses and says how to opt in
  to the file store. It never falls back on its own.
- **File** (`token_store = "file"`, opt-in). A `0600` file per profile under
  `$XDG_STATE_HOME/loomux/`, in a `0700` directory. `loomux login` stores
  there without asking for a keyring, and every start prints a one-line
  warning that the token is in a plain file. A file with wider permissions
  is refused, not silently used.
- Whatever the store, "dropping the token" (logout, a `401` with the stored
  token) deletes it from that store.
- **Lifetime** (decided 13): a linked terminal session has the same 30-day
  sliding TTL and 90-day maximum age as a web session. There's no separate
  setting.
- The config file never holds a token, and the token is never printed or
  logged. Requests go over HTTPS only (an `http://` server URL is refused
  except for loopback).
- **Revocation.** `loomux logout` calls `POST /api/v1/logout` (which also ends its
  streams and holds) and deletes the stored token. From the web, the
  sessions page revokes the labelled row (`DELETE /api/v1/sessions/{id}`).
  A `401` to a request made with the **stored** token makes the terminal
  drop the stored token and say "signed out: run `loomux login`". The link
  flow's poll is exempt. It runs only inside `loomux login`, with the
  pending `T` that isn't in the token store yet, and there a `401` just means
  "not approved yet". The poll never touches the stored entry until it gets
  a `200`.

## Attach mechanics

### Choosing the command

From the takeover response's `attach.attach_commands`, in the server's order
(the plugin's first, then `ssh`), the client takes the first whose program is
on `PATH` and that the config doesn't disable. With S8 each entry also has
`argv`, which is executed directly with no shell. Before S8, the string is
split with a POSIX word splitter, never `sh -c`.

A `kind: local` target's `attach_commands` is empty: `api/machines.go` adds
the ssh form only for remote targets, and plugin commands only for plugin
machines. For a local target the client uses attach-info's single
`attach_command` (`tmux -L <socket> attach -t <session>`, from
`targets.AttachCommand`), and only after checking it runs on the server's
own host (below).

| Target | Command | Needs on the user's machine |
|---|---|---|
| Remote (ssh) | `ssh [-p N] user@host -t 'tmux -L <socket> attach -t <session>'` | the user's own key accepted on the host; the host in their `known_hosts` (managed targets use Loomux's key, which never leaves the server) |
| Kubernetes plugin machine | `kubectl -n <ns> exec -it <pod> -- tmux -L <socket> attach -t <session>` | a kubeconfig with exec rights in that namespace; `[attach.kubectl] context = "…"` in the config adds `--context` |
| Docker plugin machine | `docker [-H ssh://…] exec -it <container> tmux -L <socket> attach -t <session>` | access to that Docker engine (locally, or ssh to its host) |
| Local (`kind: local`) | `attach.attach_command`: `tmux -L <socket> attach -t <session>` | only works on the server's own host: the client checks that the `tmux -L <socket>` server is reachable and has the session (`tmux -L <socket> has-session -t <session>`). Elsewhere it says so, shows the command and releases. Off in the container image anyway |

If the chosen command fails fast (exit status 255 from ssh, or non-zero
within 2 s), the client offers the next one and then releases. Each run
prints one line first: `attaching to cc-hub via kubectl, detach with
<prefix> d`.

### Suspending and resuming the TUI

Bubble Tea's `tea.ExecProcess` hands the terminal to a child: it leaves the
alternate screen, restores cooked mode, runs the child with the real
stdin/stdout/stderr, and restores everything when it exits. The hold's
heartbeat runs in its own goroutine owned by the attach routine, not in the
Bubble Tea update loop, so it keeps running while the TUI is suspended. When
the child exits, the client releases, resumes and refetches.

The attach routine is also a subcommand, `loomux attach [--watch]
<task|conversation>` (takeover → exec → heartbeat → release, no TUI).
`--watch` takes a watch hold and runs the read-only command. That gives two
modes:

- **inline** (default outside tmux): the TUI suspends and the attach takes
  the terminal.
- **window** (decided 3: the default inside tmux, `$TMUX` set): the TUI runs
  `tmux new-window -n <workspace> 'loomux attach [--watch] --task <id>'` in
  the user's own tmux. The list stays visible in its window, and the attach
  window holds and releases on its own.

**Detaching** (decided 4) uses tmux's own `prefix d`. Inside the user's
tmux, the remote tmux's prefix is pressed twice (`C-b C-b d`). Loomux binds
no root key on its tmux server, so every key still reaches the agent.

## Architecture

### Language and framework: Go + Bubble Tea

- **Go**, because the server is Go. The client's contract tests can import
  the server module at a pinned version and run the real `api.Server`
  in-process (see "Repository"). The same toolchain, CI conventions, vuln
  scanning and reviewers apply. And it ships as one static binary.
- **Bubble Tea** (`charmbracelet/bubbletea`) with Bubbles (list, viewport,
  textarea, spinner), Lip Gloss (layout and colour) and Glamour (markdown).
  Reasons: `ExecProcess` is exactly the suspend/resume attach needs; its
  Elm-style model makes each screen a pure update function that can be
  tested without a terminal (`teatest` golden output); and it's the most
  used Go TUI stack, which matters because, as with the web, this code will
  mostly be extended by agents. The major version is whichever is current
  and stable at R1. Only `ExecProcess`, `WithAltScreen` and the standard
  bubbles are relied on.
- **Alternatives weighed.** `tview` (Go, widget-based) has
  `Application.Suspend` too, but its retained widget tree is harder to test
  and fits a live stream less well. Rust/ratatui is good but can't share a
  toolchain or in-process tests with the server. Python/Textual needs a
  runtime on every machine. Not a close call, so it isn't an open question.
- **Small own pieces instead of dependencies.** The SSE reader (about 100
  lines: `event:`/`data:` lines, heartbeats, reconnect with backoff, matching
  `api/streams.go`) and the attach-command word splitter.
  Other deps: `zalando/go-keyring`, `BurntSushi/toml`.

### Repository: `loomux/tui` (decided: own repository, the user 2026-11-05)

The client lives in its own repository in the loomux GitHub organisation,
**`loomux/tui`**, with module path `github.com/Loomux/tui` (the
organisation's spelling in `github.com/Loomux/server`). This is the same
arrangement as `loomux/web`. This design doc and every API change stay in
`loomux/server`, because that's where the API is.

Bootstrap settings, copied from `loomux/server` and `loomux/web`: public
visibility, the MIT `LICENSE`, a ruleset protecting `main`, an immutable
`v*` tag ruleset (no update or delete), and the organisation's
allowed-actions policy (loomux, GitHub and verified-creator actions only).
It gets a `CLAUDE.md`/`AGENTS.md` with this repo's cross-workspace rules
and its own agent-mailbox name.

#### How it pins and tests against the server's API

- **A hand-written client, not a generated one.** The server publishes no
  OpenAPI document to generate from. Its machine-readable contract is
  `api/testdata/api-v1-contract.txt` (routes, JSON fields, statuses, query
  parameters and headers, generated by `TestAPIv1Contract`). The client is a
  small typed package (`client/`) whose structs hold only the fields it
  reads, as the web's TypeScript types do.
- **The pin is a Go module requirement.** `go.mod` requires
  `github.com/Loomux/server` at a release tag (`v0.3.N`; every server merge
  is a tag, so one always exists). Only `_test.go` files import it, so none
  of the server is linked into the binary. A pin is always a tag: CI
  refuses a pseudo-version or a `replace` directive on `main`. A local
  `go.work` pointing at a server checkout is the way to develop against an
  unmerged server change. It isn't committed.
- **Contract tests against a real test server.** S9 adds an exported helper
  to the server, `github.com/Loomux/server/api/apitest`. It starts a real
  `api.Server` over a temporary SQLite store with a stub dispatcher and a
  fake target executor, and seeds conversations, tasks, prompts and offers.
  The client's tests drive it over HTTP: every call the client makes, the
  SSE stream, the link flow, and takeover, heartbeat and release.
- **A route and field check.** The client keeps a list of the contract lines
  it depends on (`client/testdata/uses.txt`, in the contract file's own
  format). A test checks that each line is in the pinned server's
  `api-v1-contract.txt`, found with `go list -m -f '{{.Dir}}'
  github.com/Loomux/server`. Because the contract is additions-only within
  v1, a client that passes against its pinned server also works against
  every later v1 server.
- **Drift is caught early.** A scheduled CI run, nightly and non-blocking,
  repeats the contract tests against `github.com/Loomux/server@main`, so a
  server change that would break the client shows up before the next pin
  bump.
- **Order of a cross-repo feature.** The server PR merges first and gets its
  release tag. Then the client PR bumps the pin (`go get
  github.com/Loomux/server@v0.3.N`) together with the code that uses the new
  API. The client's README lists the minimum server version for each
  feature. At run time, features are detected (see "Versions and
  compatibility").
- **Attach tests** need a tmux helper of the client's own. The server's
  `internal/tmuxtest` can't be imported from another module. It is small:
  a temporary short `TMUX_TMPDIR`, a session, and cleanup.

Layout:

```
loomux/tui
  cmd/loomux/        main: subcommands login, logout, attach, open, version; no args = TUI
  client/            typed API client: requests, errors {error, code}, SSE, idempotency
  auth/              link flow, token stores (keyring, opt-in file), profile resolution
  attach/            takeover → choose command → exec → heartbeat → release
  ui/                Bubble Tea models: list, conversation, cards, composer, help
  config/            config file, profiles, defaults
  internal/tmuxtest/ the client's own tmux test helper
  changes/           changelog fragments, one per PR
  deploy/            release.sh and changelog.sh, adapted from loomux/server's
  docs/install.md
```

### Config

`$XDG_CONFIG_HOME/loomux/config.toml` (macOS: `~/Library/Application
Support/loomux/` if `XDG_CONFIG_HOME` is unset):

```toml
default_profile = "test"
token_store = "keyring"  # or "file": opt-in 0600 token file where there's no keyring

[profiles.test]
server = "https://loomux.example.net"

[profiles.prod]
server = "https://loomux.example.org"
warn = true            # coloured header; send, attach and take back ask first

[attach]
mode = "auto"          # auto (window inside tmux, else inline) | inline | window
prefer = ["kubectl", "ssh", "docker"]

[attach.kubectl]
context = ""           # adds --context when set

[keys]                 # any binding, e.g. attach = "A", send = "ctrl+s"
```

`LOOMUX_PROFILE` and `--profile` select the profile, and `LOOMUX_SERVER`
overrides the URL. The config never holds a token: `token_store` only
chooses where it's kept.

**Production profile** (decided 10): a profile with `warn = true` gets a
header in a warning colour naming the profile. Sending a message,
attaching (drive or watch) and taking back each ask "on `prod`: continue?
[y/N]" first. Reading needs no confirmation.

**Keybindings** (decided 1): vim-style navigation (`j/k`, `g/G`, `/`
filter, `enter` open, `esc` back) plus single-letter actions (`n` new, `a`
attach, `w` watch, `y/n` answer, `x` cancel, `o` browser, `p` transcript,
`?` help). In the composer, `enter` sends and `alt-enter` adds a newline.
Every binding is an action name in one keymap, so `[keys]` can override
any of them from the start, and `?` shows the effective map. A `[keys]`
entry that names an unknown action, or gives two actions the same key on
one screen, is an error at start rather than silently ignored.

**Layout** (decided 2): full-screen stacked screens (list → conversation →
transcript) in v1, usable in a narrow tmux pane. A two-pane layout for
140+ columns is a later ticket (L5), only if it's missed.

**Binary name** (decided 9): `loomux`.

### Versions and compatibility

On start the client calls `GET /api/v1/version`. A different `api_version` gives a
persistent warning, as the web does. Features are detected rather than
version-checked: a `404` from `POST /api/v1/tasks/{id}/takeover` means "this server can't attach
yet; upgrade it", and the client falls back to showing the attach command as
text, like the web today.

**Watch needs a positive answer, never a silent default.** The server's
JSON decoding ignores unknown fields, so a takeover route that didn't know
`mode` would grant a *drive* hold with writable commands to a client asking
to watch. Two guards close that:

- **Server (S3):** the takeover route defines `mode` from its first release.
  It accepts only `drive` and answers `400 unsupported_mode` for `watch`
  until S10 enables it. So no released server ignores the field.
- **Client (T13):** after a `mode: "watch"` takeover, the client checks
  that the returned `hold.mode` is `watch` before running any command.
  Anything else, or `400 unsupported_mode`, makes it release the hold it got
  (if any) at once and say "this server can't watch yet: upgrade it, or
  attach to drive". It never falls back to running the command it was
  given. `409 watch_unsupported` (S11: the target's plugin has no read-only
  command) gets the same message, naming the plugin.

### Releases and versioning

These follow `docs/release/versioning.md`, as a third independent version
line next to the server's and the web's:

- **Its own SemVer line, starting at 0.1.0.** Every merge to `main` is a
  patch pre-release (0.1.0, 0.1.1, …), reserved by a `deploy/release.sh
  reserve` adapted from the server's: the highest `vX.Y.Z` tag plus one,
  created on the merge commit through the GitHub API. A failed build burns
  its number, and versions are never reused.
- **MINOR is manual, one per milestone**, cut by a PR that folds `changes/`
  into `CHANGELOG.md` (`deploy/changelog.sh release VERSION DATE`). The
  merge commit is tagged by hand. 0.x lasts until the user's go for 1.0.0.
- **Changelog fragments:** each PR with a user-visible change adds
  `changes/<ticket>-<topic>.md` with Keep a Changelog sections. CI runs
  `deploy/changelog.sh check`, as in the server.
- Every 0.y.z is a GitHub **pre-release**, never marked "latest".
- `loomux version` prints the client's version, the server version it was
  pinned to, and, when signed in, the server's `GET /api/v1/version`. The
  pin can't come from the build's module info: only `_test.go` files import
  the server, so it isn't in the binary's `debug.ReadBuildInfo`. Instead,
  `go generate` writes it into a checked-in constant
  (`internal/version/serverpin.go`, from `go list -m -f '{{.Version}}'
  github.com/Loomux/server`). CI fails if that constant doesn't match
  `go.mod`, so it's right in release builds and in `go install` builds
  alike (R1 adds it, R2 checks it). The client's own version is stamped by
  the release workflow with `-ldflags -X` (R3). A `go install` build reports
  the module version from its build info instead.
- **This third line is new to `docs/release/versioning.md`**, which today
  says "Two independent version lines". R3 updates that doc in
  `loomux/server` (one PR there): three lines, the client's not reported by
  `GET /api/v1/version` and not pinned by the server. Until then, this
  design is the authority for the client's line.
- The server doesn't pin or report the client. Unlike the web, the client
  isn't served by `loomuxd`. Compatibility is by API version plus feature
  detection.

### Packaging

- The release workflow in `loomux/tui` builds on every reserved tag:
  `CGO_ENABLED=0` binaries for linux/darwin × amd64/arm64, uploaded as
  release assets with `SHA256SUMS` and a build provenance attestation (as the
  server's images have since LOOM-194).
- Not baked into the server or agent images. A hub pod doesn't need it: the
  user runs it on their own machine.
- `docs/install.md` in `loomux/tui` (linked from its README and from
  `loomux/server`'s `docs/deploy/`) covers download, checksum and
  attestation check, putting it on `PATH`, and `loomux login`. `go install
  github.com/Loomux/tui/cmd/loomux@vX.Y.Z` works too. Homebrew/AUR are
  deferred.

## Error handling

- Every API error shows the server's `error` text and branches on `code`
  (`conversation_busy`, `task_held`, `hold_ended`, `task_inactive`,
  `unauthorized`). Nothing is swallowed. An unknown code is treated as its
  status's default.
- `401` to a request made with the stored token → stored token deleted, "signed out" screen with the login command. (The link poll's `401` is "not approved yet", see "Storage and revocation".)
- Stream drop → reconnect with backoff (1 s doubling to 15 s), with a
  `reconnecting…` header after 5 s. On reconnect the conversation is
  refetched, since `message_added` isn't replayed.
- A heartbeat that keeps failing while attached can't be shown (the child
  owns the screen). After the child exits, the client reports what happened
  ("your hold expired at 14:31; the session was detached").
- Proxy interception (an HTML login page instead of JSON) → "the request was
  stopped by the auth proxy: is the token sent? Is the server URL right?"
  rather than a JSON parse error.
- Terminal too small (under 60×15) → a single line asking for more room, not
  a broken layout.

## Testing

- **UI models:** pure update tests per screen, plus `teatest` golden frames
  for the list, a needs-attention card, an offer card and the chat view at
  80×24 and 120×40.
- **Client contract:** the client runs against a real `api.Server` from
  the pinned server module, through `api/apitest` (S9): a temporary SQLite
  store, a stub dispatcher and a fake target executor. That catches field
  drift the way the web's mocked `fetch` can't. Alongside it, the
  `uses.txt` check runs against the pinned contract file, and a nightly run
  goes against the server's `main` (see "Repository").
- **Link flow:** against the same test server: 401 while pending, 200 after
  `POST /api/v1/sessions`, the code computed identically by both sides (a shared
  test vector), and refusal of a link from a linked session.
- **Attach:** the client's own tmux helper (a short `TMUX_TMPDIR`) and a fake
  attach command (a script that attaches to a local tmux session). Cases:
  takeover → exec → detach → release. `kill -9` of the client → the hold
  expires → clients detached → status `awaiting_input`. A forced takeover
  from a second session detaches the first. A watch runs `attach -r` and
  leaves the status alone. Its expiry detaches nobody. A watch coexists
  with a drive hold, and upgrading a watch to a drive detaches the read-only
  client first.
- **Keymap and profiles:** `[keys]` overrides apply, an unknown action or a
  clashing key is an error at start, and a `warn` profile asks before send,
  attach, watch and take back.
- **Server:** TDD with `-race` on the new store methods (storetest
  conformance), the routes, the sweeper (expiry, restart grace, revocation)
  and S1's compare-and-set writes (a takeover during a wait survives the
  wait's end). Watch holds: many per task, no status change, read-only
  commands from every source, and `409 watch_unsupported` for a plugin
  without the capability.
- **End to end on TEST (T12, needs the user for MFA):** link through the
  proxy, watch then attach to a Kubernetes machine, take back from the
  phone, crash the terminal, deploy while attached.

## Changes outside the client

| # | Where | Change |
|---|---|---|
| S1 | server | compare-and-set status writes after waits (takeover can't be overwritten) |
| S2 | server | `task_holds` table, store methods, storetest conformance |
| S3 | server | `takeover` / `heartbeat` / `release` routes (with `mode`, only `drive` accepted until S10), contract, README |
| S4 | server | expiry sweeper, restart grace, revocation ends holds, `DetachClients` |
| S5 | server | takeover of a running turn ends its wait (`taken_over`) |
| S6 | server | `hold` on tasks and attach-info, `held_by` + `pending_confirmation_id` on list rows, `task_hold` event, error class `held` |
| S7 | server | session `label`/`client_kind`/`origin`, `POST /api/v1/sessions` (login-origin only) |
| S8 | server | `argv` on `attach_commands` (plugin protocol + ssh form) |
| S9 | server | exported test helper `api/apitest` for the client's contract tests |
| S10 | server | watch holds (`mode: watch`, many per task, no status change, no detach on expiry), `watchers` on list rows |
| S11 | server | read-only attach commands: `read_only` in the plugin protocol's `AttachParams` under a new capability, kubernetes + docker `attach -r`, server-built ssh/local forms, `409 watch_unsupported` |
| W1 | web | `/link` page |
| W2 | web | holder banner + Take back, sessions labels, "open in terminal" command |
| W3 | web | watchers shown next to the holder |
| R3 | server docs | `docs/release/versioning.md`: a third, independent version line for `loomux/tui` |

`core-design.md` §4 (takeover/release) and §9 (client API) and
`api/README.md` are updated with the server tickets that change them.

## Build plan

Tickets of at most one day. The estimate is focused work including tests and
review fixes.

**Phase 1: server foundations (blocks attach)**

| Ticket | Scope | Est. | Depends on |
|---|---|---|---|
| S1 | CAS status writes in the orchestrator and router paths that write after a wait | 1 d | — |
| S2 | `task_holds` migration + store + conformance | 1 d | — |
| S3 | takeover/heartbeat/release routes (hold checked before status; failed-task holds; `mode` defined, `watch` → `400 unsupported_mode` until S10), orchestrator wiring, error codes, contract | 1 d | S1, S2 |
| S4 | sweeper, restart grace, revocation, `TargetExecutor.DetachClients` | 1 d | S3 |
| S5 | running-turn takeover ends the dispatch wait (`taken_over`) | 1 d | S3 |
| S6 | expose holds (task, attach-info, list row, `task_hold` event), pending offer on list rows, error class `held` | 1 d | S3 |
| S7 | session labels/origin + `POST /api/v1/sessions` | 1 d | — |
| S8 | `argv` on attach commands (plugin protocol field, kubernetes, docker, ssh) | 0.5 d | — |
| S9 | `api/apitest`: exported real-`api.Server` test helper (temporary SQLite, stub dispatcher, fake executor, seed helpers), for `loomux/tui`'s contract tests | 1 d | — (extended by S3/S6/S7 as their routes land) |
| S10 | watch holds: `mode: watch` accepted on takeover, many per task beside one drive hold, status untouched, expiry detaches nobody, `watchers` on list rows, `task_hold` for both modes | 1 d | S3, S6 |
| S11 | read-only attach commands: `read_only` in `AttachParams` + capability flag, kubernetes and docker plugins, ssh and local forms, `409 watch_unsupported` | 1 d | S8, S10 |

**Phase 2: web (in loomux/web)**

| Ticket | Scope | Est. | Depends on |
|---|---|---|---|
| W1 | `/link` page: fragment, code, approve | 1 d | S7 |
| W2 | holder banner, Take back, session labels, open-in-terminal | 1 d | S6, S7 |
| W3 | watchers next to the holder (list rows and conversation) | 0.5 d | S10, W2 |

**Phase 3: the client (in loomux/tui)**

| Ticket | Scope | Est. | Depends on |
|---|---|---|---|
| R1 | repo bootstrap: the user creates `loomux/tui` in the org; settings as in "Repository" (public, MIT, `main` ruleset, immutable `v*` tags, allowed-actions policy); `go.mod` (`github.com/Loomux/tui`) pinning a server tag; `cmd/loomux` skeleton with `version`, the generated server-pin constant (`go generate`); README, `CLAUDE.md`/`AGENTS.md` | 1 d | — |
| R2 | CI: build, `vet`, `gofmt -l`, `go test -race`, `govulncheck` (pinned toolchain), changelog check, "pin is a tag, no `replace`" check, server-pin constant matches `go.mod`, the `uses.txt` contract check, nightly non-blocking run against server `main` | 1 d | R1 |
| R3 | release: `deploy/release.sh reserve` + `deploy/changelog.sh` adapted from the server, release workflow (binaries with the version stamped by `-ldflags -X`, `SHA256SUMS`, attestation, GitHub pre-release), first 0.1.0; a PR in `loomux/server` updating `docs/release/versioning.md` for the third version line | 1 d | R2 |
| T1 | config file + profiles, keyring store and the opt-in `0600` file store, the HTTPS-only rule | 1 d | R1 |
| T14 | keymap: action names, vim-style defaults, `[keys]` overrides, start-up validation, `?` help from the effective map | 0.5 d | T1 |
| T2 | `login` / `logout` link flow, 401 handling | 1 d | T1, S7, W1 |
| T3 | `client/`: typed calls, errors, SSE reader, idempotency; contract tests through `api/apitest`; `uses.txt` | 1 d | R1, S9 |
| T4 | home list: ranking, filters, polling, row rendering | 1 d | T3, T14 |
| T5 | conversation view: transcript, markdown, live stream, task status | 1 d | T3 |
| T6 | composer, send (async + idempotency), busy handling, cancel | 1 d | T5 |
| T7 | attention, offer and failed-task (`login_required`, `agent_rate_limited`, `agent_api_error`) cards, answering keys | 0.5 d | T5 |
| T8 | `loomux attach`: takeover, command choice, exec, heartbeat, release, `Pdeathsig` | 1 d | T3, S3, S4 |
| T9 | attach from the TUI: inline suspend/resume, tmux window mode, failure fallback | 1 d | T8 |
| T10 | handoff: `loomux open`, open-in-browser, holder in rows, take back from the TUI | 0.5 d | T4, S6 |
| T11 | `docs/install.md` (download, checksum and attestation check, `go install`), the README's minimum-server-version table, link from `loomux/server` `docs/deploy/` | 0.5 d | R3 |
| T13 | watch: `w` and `loomux attach --watch`, read-only command, the `hold.mode` check and release-on-mismatch fallback (`unsupported_mode`, `watch_unsupported`), upgrade a watch to a drive, watchers in rows | 1 d | T9, S10, S11 |
| T15 | `warn` profiles: coloured header, confirmation before send, attach, watch and take back | 0.5 d | T6, T9, T10 |
| T12 | end-to-end on TEST with the user (link via MFA, k8s watch and attach, crash, deploy, phone take-back) | 1 d | all above |

**Later (not needed for LOOM-235)**

| Ticket | Scope | Est. |
|---|---|---|
| L1 | cross-conversation stream `GET /api/v1/stream` (`conversation_update`, `task_hold`), replacing list polling in TUI and web | 1 d |
| L3 | transcript and event (audit trail) views in the TUI | 1 d |
| L4a | server-proxied attach, server side: `GET /api/v1/tasks/{id}/terminal` WebSocket upgrade, held by the caller's hold, running `tmux attach` over the target's executor in a PTY (decided 5: not in v1, only if wanted later) | 1 d |
| L4b | the same route's resize and close frames, the hold's end closing the socket, and tests through `internal/tmuxtest` | 1 d |
| L4c | the client's `proxy` attach mode: raw terminal ↔ WebSocket, used when no local attach command works | 1 d |
| L5 | two-pane layout (list left, conversation right) at 140+ columns, only if it's missed (decided 2) | 1 d |

(L2, watch mode, moved into v1 as S10, S11, W3 and T13.)

Totals: server 10.5 d, web 2.5 d, client 15.5 d (repository 3 d, features
12.5 d), about **28.5 days** for v1. The later tickets (L1, L3, L4a–c, L5,
6 d) aren't counted. The LOOM-235 cutover needs phases 1–3, watch mode
included (decided 12). Of those, only T10 (handoff conveniences) and W2's
"open in terminal" text could slip past it without losing a workflow.
Server phase 1 and R1–R3/T1/T14 can run in parallel.
T3 starts once S9 is released, and each client ticket that uses a new route
bumps the server pin to the release that has it.

## Decided

All open questions were answered by the user on 2026-11-05 ("all as
proposed + your suggestions"; the repository one earlier the same day). The numbers are the
original question numbers, which the text above cites as "decided N".

1. **Keybindings:** vim-style navigation plus single-letter actions as
   proposed. In the composer, `enter` sends and `alt-enter` adds a newline.
   Everything is overridable in `[keys]` from the start (T14).
2. **Layout:** full-screen stacked screens in v1. The two-pane layout at
   140+ columns is a later ticket (L5), only if it's missed.
3. **Attach inside tmux:** a new tmux window (`window` mode). Outside tmux
   the TUI suspends inline.
4. **Detach key:** tmux's own `prefix d`. Loomux binds no root key.
5. **Server-proxied attach:** not in v1. L4a–c stay later and outside the
   v1 total.
6. **Taking over a running turn:** allowed. The turn ends as `taken_over`
   and what the agent finishes is relayed later (S5).
7. **Lease:** 45 s TTL, 15 s heartbeat. On expiry the session's tmux
   clients are detached (drive holds. A watch hold's expiry detaches
   nobody).
8. **Repository:** its own repository, `loomux/tui`. This design doc stays
   in `loomux/server`'s `docs/design/` with the API changes it specifies.
9. **Binary name:** `loomux`.
10. **Production profile:** a `warn` profile gets a coloured header, and
    send, attach (and watch) and take back ask for confirmation there (T15).
11. **No keyring:** the opt-in `0600` token file (`token_store = "file"`) is
    allowed (T1).
12. **Watch mode:** in v1, needed for the cutover (S10, S11, W3, T13).
13. **Linked terminal sessions:** the same lifetime as the web, 30-day
    sliding and 90-day maximum.

## Open questions for the user

None open. New ones raised during the build go to command-center for the
user.

## Checks before building (facts, not preferences)

- The proxy's bypass rule matches on the *presence* of a bearer token for
  `/api/v1/` (it can't validate Loomux tokens), so the link flow's polling
  with a not-yet-approved token reaches `loomuxd`. Confirm on TEST with one
  `curl` before T2.
- SSE through the proxy with a bearer token already works for the web.
  Confirm it also does for a non-browser client (no cookies) during T3.
- Which tmux clients `detach-client -s` reaches on a plugin machine (the
  machine's own tmux server on the instance socket, LOOM-210). Verify in S4's
  test.
- Nothing calls `Orchestrator.Takeover` today, so no task should be in
  `human_takeover` without a hold. S2's migration checks and logs any that
  are.

## Deferred

- Windows support, desktop notifications, Homebrew/AUR packages.
- A QR code of the link URL for approving from a phone.
- Composer drafts shared across clients.
- Admin screens (targets, plugins, router settings).
