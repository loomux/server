# Loomux — Terminal Client Design (LOOM-261)

**Status:** draft for review, 2026-11-05 · **Ticket:** LOOM-261 (Vikunja Loomux id 2359, GitHub #488)
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
  to the OS keyring and can be revoked from the web or the terminal.
- One static binary for Linux and macOS.

**Non-goals (v1)**
- Administration: targets, plugins, machines, SSH keys, credentials and
  router settings stay in the web. The terminal may link to the web page.
- Windows. It may build, but attach and the keyring aren't tested there.
- Desktop notifications. The existing "needs you" notifications already link
  to the web.
- Read-only "watch" attach (a later phase, see the build plan).
- Server-proxied attach over a WebSocket (open question 5).

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
  `POST /dispatch` with `confirmation_id` and "yes" or "no" (LOOM-123). There
  are no dedicated approve/deny routes and none are needed.
- **Attach info gives shell strings.** `GET /tasks/{id}/attach-info` returns
  `attach_commands: [{via, command}]`: the plugin's (`kubectl … exec -it …`,
  `docker [-H …] exec -it …`) and the plain `ssh` form for every remote
  target. These are strings meant for a person to copy. A program has to
  split them, so an `argv` form is proposed (S8).
- **There's no feed across conversations.** The SSE stream is per
  conversation. The web's list ranks `GET /conversations` by the latest task's
  status. A conversation that ended on an offer awaiting a yes doesn't show
  as needing the user there (S6).
- **Sessions carry no label.** `GET /sessions` shows `{id, created_at,
  last_used_at, current}`, so a "your devices" view can't say which row is the
  terminal (S7).
- `/login` takes a password, and the deployment only exposes it through the
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
 [n]ew  [enter] open  [a]ttach  [/] filter  [tab] needs-you/running/done/all
```

- Ranked like the web: `needs_attention`, `awaiting_input` and
  `human_takeover` first, then `running`, then `completed`/`failed`. Newest
  first within a group, `conversation_id` as tie-breaker. Conversations with
  a pending offer also rank first once S6 exposes it.
- Each row shows the status glyph, workspace, preview (the first message),
  the holder if held, and age.
- Live: v1 polls `GET /conversations?limit=100` every 5 s and right after any
  action. The cross-conversation stream (L1) replaces polling later.
- `a` on a row attaches to that conversation's open agent task (workflow 4).

### 2. Chat with a live stream

Opening a conversation loads `GET /conversations/{id}` (messages, tasks,
dispatches, confirmations) and opens `GET /conversations/{id}/stream`.

- `message_added` → refetch the conversation. `dispatch_update` → update the
  in-flight turn card and, on `succeeded`, show the reply. `task_update` →
  update the task's status line.
- Sending is async `POST /dispatch` with a fresh `Idempotency-Key` per
  submit, reused on retry (as the web does since LOOM-81). `409
  conversation_busy` puts the turn named by its `dispatch_id` in view rather
  than showing an error.
- `ctrl-c` (or `x`) on an in-flight turn → `POST /dispatches/{id}/cancel`,
  after a confirm.
- Replies are markdown and are rendered as markdown (glamour). The composer
  is multi-line. `enter` sends and `alt-enter` adds a newline (open question
  1).
- `n` from the list starts a conversation (empty `conversation_id`, the
  server mints one). Starting from a workspace sends that `workspace_hint`.
- `p` on a task shows its transcript (`GET /tasks/{id}/transcript`, paged):
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
keys for the agent itself. `kind: login` or `usage_limit` shows the card with
attach as the only action, since a login is the user's to do in the pane.
An offer card (pending confirmation) has `y`/`n`, which send "yes"/"no" with
its `confirmation_id`. A stale offer is refused by the server and shown as
such.

### 4. Attach to a live agent and detach

```
 list/chat ──a──► POST /tasks/{id}/takeover ──► hold + attach info
                         │
                         ▼
        suspend the TUI, run the attach command on this terminal
        (heartbeat goroutine renews the hold every 15 s)
                         │   user works in Claude Code / Codex natively
                         ▼
        attach command exits (detach, ssh drop, pane gone)
                         │
                         ▼
        POST /tasks/{id}/release {hold_id} ──► resume the TUI, refresh
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
| `mode` | `drive` (v1). `watch` is reserved for L2 |
| `prev_status` | the task's status when taken, for display and audit |
| `acquired_at`, `renewed_at`, `expires_at` | |
| `ended_at`, `ended_reason` | `released`, `expired`, `taken_back`, `taken_over`, `session_revoked`, `task_ended` |

New table `task_holds` (migration `00036`), one active `drive` hold per task
(a partial unique index on `task_id` where `ended_at IS NULL AND mode =
'drive'`). Ended rows are kept for the audit trail and swept by retention
like confirmations.

The hold is the source of truth for *who*. The task's status
`human_takeover` remains the source of truth for *whether automation may
type*, so every existing gate keeps working unchanged.

### API (additions within v1)

- `POST /api/v1/tasks/{id}/takeover` `{client_label?, ttl_seconds?, force?}`
  → `200 {task_id, hold, attach}`. `attach` is the attach-info body, so one
  round trip gets both. `ttl_seconds` is 15–300, default 45.
  - The task must be `running`, `awaiting_input` or `needs_attention`. Status
    moves to `human_takeover` by compare-and-set. An ended task is `409`
    (`task_inactive`).
  - Held by another session: `409 task_held` with the `hold`, unless `force`.
    `force` ends the other hold as `taken_over` and detaches its clients.
  - Held by this same session: the existing hold is renewed and returned
    (idempotent, and how a restarted TUI gets its own hold back).
- `POST /api/v1/tasks/{id}/takeover/heartbeat` `{hold_id}` → `200 {hold}` with
  `expires_at` pushed out by the TTL. An ended hold is `410 hold_ended` with
  `ended_reason`.
- `POST /api/v1/tasks/{id}/release` `{hold_id?}`:
  - With `hold_id`, ends that hold as `released`. Idempotent: an
    already-ended hold answers `200` with its reason.
  - Without it, it's **Take back**: ends whatever drive hold is active as
    `taken_back` and detaches its clients. Single user, so any session may.
  - Either way the task goes `human_takeover` → `awaiting_input` (as
    `Orchestrator.Release` does today) and its stale `attention` is cleared.
    Anything the agent finished while driven is relayed by the existing
    late-output pass, whose own comment already covers "a task handed back
    from a human takeover" (`router/lateoutput.go`).
- Exposure (S6): `hold` (active or null) on each task of
  `GET /conversations/{id}` and on attach-info. On `GET /conversations` rows:
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
  `expired`, releases their tasks, then **detaches the session's tmux
  clients** (`tmux -L <socket> detach-client -s <session>` through the
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
- **Session revoked** (`/logout`, `DELETE /sessions/{id}`, expiry): its holds
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

Open question 6 asks whether the user prefers "refuse until the turn ends"
instead.

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
reach `/login`. It can't reach an unauthenticated "start device flow"
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
3. The web's `/link` page (W1) goes through the proxy's MFA and the web's own
   login as usual. It shows the label and `V`, says "approve only if your
   terminal is showing this code right now", and on **Approve** calls
   `POST /api/v1/sessions {token_hash: H, label, client_kind: "tui"}` with the
   web's own token (S7).
4. Meanwhile the terminal polls `GET /api/v1/sessions` with `Authorization:
   Bearer T` every 2 s for up to 10 minutes. The bearer header gets it past
   the proxy, and it gets `401` until the session exists, then `200`.
5. It stores `T` in the OS keyring and is done. `T` itself never left the
   machine.

Why not a classic device-code flow (RFC 8628): its first call is
unauthenticated, which the proxy blocks. Exempting a path would put a new
unauthenticated, proxy-bypassing endpoint on the internet. In this flow every
call carries a bearer token, and the only new server surface is an
authenticated route.

Server side (S7):
- `sessions` gains `label`, `client_kind` (`web`, `tui`, `app`, `""`) and
  `origin` (`login` or `link`). `GET /sessions` returns the first two.
- `POST /api/v1/sessions` accepts a 64-hex `token_hash`, a `label` (≤ 64
  printable characters) and a `client_kind`. `409` if the hash exists. It is
  **allowed only from a session whose origin is `login`**: a stolen terminal
  token can't mint itself successors that survive its revocation. A linked
  session has the same sliding TTL and 90-day max age as any other.
- `/login` sets `origin: login` and `client_kind: web` (the web is its only
  caller).

Phishing note: as with any device flow, someone could send the user a link
carrying their own `H`. The code comparison and the page's wording are the
defence, and the sessions page shows every linked terminal for revocation.
For a single-user tool that's proportionate.

`GET /api/v1/version` is unauthenticated, but the proxy would still intercept
it without a bearer header. The terminal sends its stored token (or, during
login, the pending `T`) on every call, `/version` included, and the handler
ignores it.

### Storage and revocation

- **Keyring.** Service `loomux`, account `<profile>@<server URL>`, through
  `github.com/zalando/go-keyring`: Secret Service over D-Bus on Linux
  (GNOME Keyring, KWallet, KeePassXC), the Keychain on macOS. Pure Go, no cgo.
- **No keyring available** (a headless box, a container): `loomux login`
  refuses and says why. A `0600` file under `$XDG_STATE_HOME/loomux/` is used
  only if the config says `token_store = "file"`, with a warning at every
  start (open question 11).
- The config file never holds a token, and the token is never printed or
  logged. Requests go over HTTPS only (an `http://` server URL is refused
  except for loopback).
- **Revocation.** `loomux logout` calls `POST /logout` (which also ends its
  streams and holds) and deletes the keyring entry. From the web, the
  sessions page revokes the labelled row (`DELETE /sessions/{id}`). Any `401`
  makes the terminal drop the keyring entry and say "signed out: run
  `loomux login`".

## Attach mechanics

### Choosing the command

From the takeover response's `attach.attach_commands`, in the server's order
(the plugin's first, then `ssh`), the client takes the first whose program is
on `PATH` and that the config doesn't disable. With S8 each entry also has
`argv`, which is executed directly with no shell. Before S8, the string is
split with a POSIX word splitter, never `sh -c`.

| Target | Command | Needs on the user's machine |
|---|---|---|
| Remote (ssh) | `ssh [-p N] user@host -t 'tmux -L <socket> attach -t <session>'` | the user's own key accepted on the host; the host in their `known_hosts` (managed targets use Loomux's key, which never leaves the server) |
| Kubernetes plugin machine | `kubectl -n <ns> exec -it <pod> -- tmux -L <socket> attach -t <session>` | a kubeconfig with exec rights in that namespace; `[attach.kubectl] context = "…"` in the config adds `--context` |
| Docker plugin machine | `docker [-H ssh://…] exec -it <container> tmux -L <socket> attach -t <session>` | access to that Docker engine (locally, or ssh to its host) |
| Local (`kind: local`) | `tmux -L <socket> attach -t <session>` | only works on the server's own host. Elsewhere the client says so and shows the command. Off in the container image anyway |

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

The attach routine is also a subcommand, `loomux attach <task|conversation>`
(takeover → exec → heartbeat → release, no TUI). That gives two modes:

- **inline** (default outside tmux): the TUI suspends and the attach takes
  the terminal.
- **window** (default inside tmux, `$TMUX` set): the TUI runs
  `tmux new-window -n <workspace> 'loomux attach --task <id>'` in the
  user's own tmux. The list stays visible in its window, and the attach
  window holds and releases on its own. Open question 3.

**Nested tmux.** Inside the user's tmux, the remote tmux's prefix is pressed
twice (`C-b C-b d`). Open question 4 asks whether Loomux's tmux server should
bind a dedicated detach key instead.

## Architecture

### Language and framework: Go + Bubble Tea

- **Go**, because the server is Go. The client's contract tests can run the
  real `api.Server` in-process. It can reuse `internal/tmuxtest` for attach
  tests (a nested module under `github.com/Loomux/server/` may import the
  parent's `internal/` packages). The same toolchain, CI conventions, vuln
  scanning and reviewers apply. And it ships as one static binary.
- **Bubble Tea** (`charmbracelet/bubbletea`) with Bubbles (list, viewport,
  textarea, spinner), Lip Gloss (layout and colour) and Glamour (markdown).
  Reasons: `ExecProcess` is exactly the suspend/resume attach needs; its
  Elm-style model makes each screen a pure update function that can be
  tested without a terminal (`teatest` golden output); and it's the most
  used Go TUI stack, which matters because, as with the web, this code will
  mostly be extended by agents. The major version is whichever is current
  and stable at T1. Only `ExecProcess`, `WithAltScreen` and the standard
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

### Where the code lives: this repo, as a nested module (recommended)

`tui/` with its own `go.mod` (`module github.com/Loomux/server/tui`,
`replace github.com/Loomux/server => ../`), built as `tui/cmd/loomux`. This
is the same pattern as `plugins/kubernetes` and `plugins/docker`.

- **For:** server and client changes for holds and links land in one PR and
  are reviewed together. Contract tests against the real handlers. The
  charm dependencies stay out of the server's module, image and
  `govulncheck` scope.
- **Against:** the client releases with server versions (below), and CI
  needs a job for the module (as the plugins have).
- **Own repo (`loomux/tui`, like `loomux/web`):** an independent release
  line and a cleaner public face, at the cost of cross-repo PRs for every
  API addition during build-out. It could still import the server module
  for tests. Worth reconsidering after v1 stabilises (open question 8).

Layout:

```
tui/
  cmd/loomux/        main: subcommands login, logout, attach, open, version; no args = TUI
  client/            typed API client: requests, errors {error, code}, SSE, idempotency
  auth/              link flow, keyring store, profile resolution
  attach/            takeover → choose command → exec → heartbeat → release
  ui/                Bubble Tea models: list, conversation, cards, composer, help
  config/            config file, profiles, defaults
```

### Config

`$XDG_CONFIG_HOME/loomux/config.toml` (macOS: `~/Library/Application
Support/loomux/` if `XDG_CONFIG_HOME` is unset):

```toml
default_profile = "test"

[profiles.test]
server = "https://loomux.example.net"

[profiles.prod]
server = "https://loomux.example.org"
warn = true            # header shown in a warning colour (open question 10)

[attach]
mode = "auto"          # auto | inline | window
prefer = ["kubectl", "ssh", "docker"]

[attach.kubectl]
context = ""           # adds --context when set

[keys]                 # overrides, see open question 1
```

`LOOMUX_PROFILE` and `--profile` select the profile, and `LOOMUX_SERVER`
overrides the URL. There's no token setting: tokens live only in the keyring.

### Versions and compatibility

On start the client calls `GET /version`. A different `api_version` gives a
persistent warning, as the web does. Features are detected rather than
version-checked: a `404` from `/takeover` means "this server can't attach
yet; upgrade it", and the client falls back to showing the attach command as
text, like the web today.

### Packaging

- Built by a job in `image.yml`'s version pipeline on every release tag:
  `CGO_ENABLED=0` binaries for linux/darwin × amd64/arm64, uploaded as
  release assets with `SHA256SUMS` and a build provenance attestation (as the
  images have since LOOM-194). Same version number as the server. The binary
  reports it (`loomux version`).
- Not baked into the server or agent images. A hub pod doesn't need it: the
  user runs it on their own machine.
- Install doc `docs/deploy/tui.md`: download, verify, put on `PATH`,
  `loomux login`. Homebrew/AUR are deferred.

## Error handling

- Every API error shows the server's `error` text and branches on `code`
  (`conversation_busy`, `task_held`, `hold_ended`, `task_inactive`,
  `unauthorized`). Nothing is swallowed. An unknown code is treated as its
  status's default.
- `401` → keyring entry dropped, "signed out" screen with the login command.
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
- **Client contract:** the client runs against a real `api.Server` over a
  temporary SQLite store with a stub dispatcher (a small exported test helper
  in the server module, `api/apitest`, added in T3). That catches field
  drift the way the web's mocked `fetch` can't.
- **Link flow:** against the same test server: 401 while pending, 200 after
  `POST /sessions`, the code computed identically by both sides (a shared
  test vector), and refusal of a link from a linked session.
- **Attach:** `internal/tmuxtest` with a short `TMUX_TMPDIR` and a fake
  attach command (a script that attaches to a local tmux session). Cases:
  takeover → exec → detach → release. `kill -9` of the client → the hold
  expires → clients detached → status `awaiting_input`. A forced takeover
  from a second session detaches the first.
- **Server:** TDD with `-race` on the new store methods (storetest
  conformance), the routes, the sweeper (expiry, restart grace, revocation)
  and S1's compare-and-set writes (a takeover during a wait survives the
  wait's end).
- **End to end on TEST (T11, needs the user for MFA):** link through the
  proxy, attach to a Kubernetes machine, take back from the phone, crash the
  terminal, deploy while attached.

## Changes outside the client

| # | Where | Change |
|---|---|---|
| S1 | server | compare-and-set status writes after waits (takeover can't be overwritten) |
| S2 | server | `task_holds` table, store methods, storetest conformance |
| S3 | server | `takeover` / `heartbeat` / `release` routes, contract, README |
| S4 | server | expiry sweeper, restart grace, revocation ends holds, `DetachClients` |
| S5 | server | takeover of a running turn ends its wait (`taken_over`) |
| S6 | server | `hold` on tasks and attach-info, `held_by` + `pending_confirmation_id` on list rows, `task_hold` event, error class `held` |
| S7 | server | session `label`/`client_kind`/`origin`, `POST /sessions` (login-origin only) |
| S8 | server | `argv` on `attach_commands` (plugin protocol + ssh form) |
| W1 | web | `/link` page |
| W2 | web | holder banner + Take back, sessions labels, "open in terminal" command |

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
| S3 | takeover/heartbeat/release routes, orchestrator wiring, error codes, contract | 1 d | S1, S2 |
| S4 | sweeper, restart grace, revocation, `TargetExecutor.DetachClients` | 1 d | S3 |
| S5 | running-turn takeover ends the dispatch wait (`taken_over`) | 1 d | S3 |
| S6 | expose holds (task, attach-info, list row, `task_hold` event), pending offer on list rows, error class `held` | 1 d | S3 |
| S7 | session labels/origin + `POST /sessions` | 1 d | — |
| S8 | `argv` on attach commands (plugin protocol field, kubernetes, docker, ssh) | 0.5 d | — |

**Phase 2: web (in loomux/web)**

| Ticket | Scope | Est. | Depends on |
|---|---|---|---|
| W1 | `/link` page: fragment, code, approve | 1 d | S7 |
| W2 | holder banner, Take back, session labels, open-in-terminal | 1 d | S6, S7 |

**Phase 3: the client**

| Ticket | Scope | Est. | Depends on |
|---|---|---|---|
| T1 | `tui/` module, `cmd/loomux`, config + profiles, keyring store, `version`, CI job | 1 d | — |
| T2 | `login` / `logout` link flow, 401 handling | 1 d | T1, S7, W1 |
| T3 | `client/`: typed calls, errors, SSE reader, idempotency; `api/apitest` helper; contract tests | 1 d | T1 |
| T4 | home list: ranking, filters, polling, row rendering | 1 d | T3 |
| T5 | conversation view: transcript, markdown, live stream, task status | 1 d | T3 |
| T6 | composer, send (async + idempotency), busy handling, cancel | 1 d | T5 |
| T7 | attention and offer cards, answering keys | 0.5 d | T5 |
| T8 | `loomux attach`: takeover, command choice, exec, heartbeat, release, `Pdeathsig` | 1 d | T3, S3, S4 |
| T9 | attach from the TUI: inline suspend/resume, tmux window mode, failure fallback | 1 d | T8 |
| T10 | handoff: `loomux open`, open-in-browser, holder in rows, take back from the TUI | 0.5 d | T4, S6 |
| T11 | release assets + attestation job, `docs/deploy/tui.md` | 1 d | T1 |
| T12 | end-to-end on TEST with the user (link via MFA, k8s attach, crash, deploy, phone take-back) | 1 d | all above |

**Later (not needed for LOOM-235)**

| Ticket | Scope | Est. |
|---|---|---|
| L1 | cross-conversation stream `GET /api/v1/stream` (`conversation_update`, `task_hold`), replacing list polling in TUI and web | 1 d |
| L2 | watch mode: `mode: watch` holds (many per task), `tmux attach -r`, "watched by" | 1 d |
| L3 | transcript and event (audit trail) views in the TUI | 1 d |
| L4 | server-proxied attach over a WebSocket (only if open question 5 says yes; about 3 tickets) | 3 d |

Totals: server 7.5 d, web 2 d, client 11 d, about **20.5 days** for v1. The
LOOM-235 cutover needs phases 1–3. Of those, only T10 (handoff
conveniences) and W2's "open in terminal" text could slip past it without
losing a workflow. Server phase 1, T1 and T3 can run in parallel.

## Open questions for the user

These are preferences or trade-offs that are the user's to choose. The
recommendation is first.

1. **Keybindings.** Proposed: vim-style navigation (`j/k`, `g/G`, `/` filter,
   `enter` open, `esc` back) plus single letters for actions (`n` new, `a`
   attach, `y/n` answer, `x` cancel, `o` browser, `p` transcript, `?` help).
   Composer: `enter` sends, `alt-enter` adds a newline. Alternatives:
   arrow-first, or `ctrl-enter` to send. Should it be all overridable in
   `[keys]` from the start?
2. **Layout.** Proposed: full-screen stacked screens (list → conversation),
   which works in a narrow tmux pane. Alternative: two panes (list left,
   conversation right) when the terminal is 140+ columns wide.
3. **Attach inside tmux.** Proposed: open a new tmux window (`window` mode)
   so the list stays visible. Alternatives: suspend inline as outside tmux,
   or a split pane.
4. **Detach key.** Proposed: keep tmux's own (`prefix d`, doubled when
   nested), changing nothing. Alternative: Loomux binds a root key such as
   `C-]` on its own tmux socket to `detach-client`, which works from anywhere
   but takes that key from the agent.
5. **Server-proxied attach** (L4). Recommendation: not for v1. Executing the
   target's own attach command needs no new remote-shell surface. Proxying
   through `loomuxd` over a WebSocket would work without kubectl/ssh access
   on the user's machine and would later give the web and apps a real
   terminal. Wanted, and when?
6. **Taking over a running turn.** Proposed: allowed. The turn's wait ends as
   `taken_over` and what the agent finishes is relayed later. Alternatives:
   refuse until the turn ends, or cancel the turn (sending Escape) first.
7. **Lease timing and expiry.** Proposed: 45 s TTL, 15 s heartbeat, and on
   expiry the session's tmux clients are detached. Shorter TTLs release
   faster after a crash but are less forgiving on a flaky link.
8. **Repository.** Recommended: a nested module `tui/` in this repo. The
   alternative is its own repo, `loomux/tui`, like the web.
9. **Binary name.** `loomux` (matching `core-design.md`'s `loomux takeover`).
   Alternatives: `lx`, `loomux-tui`.
10. **Production profile.** Should a profile marked `warn` get a coloured
    header, and should actions there (send, attach, take back) ask for
    confirmation?
11. **No keyring.** Allow the opt-in `0600` token file on machines without a
    Secret Service, or refuse there outright?
12. **Watch mode in v1** (L2): needed for the cutover, or later?
13. **Linked terminal sessions' lifetime.** The same 30-day sliding / 90-day
    maximum as the web, or shorter?

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
