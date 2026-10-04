# Loomux — Core Design (Pre-Alpha)

## Overview

Loomux is a chat-driven interface to a fleet of on-demand, tmux-backed coding
agents. A lightweight, swappable router model sits between the chat UI and
the agent fleet: it decides which workspace a request belongs to (or whether
a new one needs provisioning), dispatches into a tmux pane, and relays the
result back to the conversation. A human can SSH into wherever an agent is
running and attach to that same tmux session to watch or take over directly,
without breaking the automated side.

This formalizes and automates a pattern already used by hand today across
this ecosystem: pasting kickoff prompts into fresh `claude` sessions across
several dedicated tmux-backed workspaces (theWyseKube, jetone-infra,
scds-infra, command-center itself), with results relayed back manually.
Loomux is that loop, productized.

## Scope

**In scope for this design (pre-alpha core):**
- The Loomux server (single binary)
- The workspace registry and pluggable storage layer
- The tmux-based agent harness: lifecycle, pane kinds, targets (local/remote)
- Completion detection
- Router-model integration (interface only, not a specific vendor)
- The hybrid credential model
- Auth for clients talking to the server
- Versioning strategy

**Explicitly out of scope for this spec** (each gets its own design pass once
this core is settled):
- Web, Android, and iOS client UIs
- GitHub org / repo layout and provisioning mechanics
- A specific router-model vendor/provider
- Multi-user support (v1 is single-user by design)
- Concrete agent-type adapters beyond the interface they must satisfy

## Architecture Overview

```
┌─────────────┐     HTTPS/WS      ┌──────────────────────────┐
│ Web / iOS /  │ ───────────────► │      Loomux Server        │
│ Android      │ ◄─────────────── │  (auth, chat API)         │
└─────────────┘                   │                            │
                                   │  ┌──────────────────────┐ │
                                   │  │   Router (LLM call)   │ │
                                   │  │ workspace select +    │ │
                                   │  │ relay/summarize        │ │
                                   │  └──────────┬───────────┘ │
                                   │             │              │
                                   │  ┌──────────▼───────────┐ │
                                   │  │  Agent Orchestrator   │ │
                                   │  │  (pane lifecycle,      │ │
                                   │  │   completion detect)   │ │
                                   │  └──────────┬───────────┘ │
                                   │             │              │
                                   │  ┌──────────▼───────────┐ │
                                   │  │ TargetExecutor         │ │
                                   │  │ interface              │ │
                                   │  └──────────┬───────────┘ │
                                   │             │              │
                                   │  ┌──────────▼───────────┐ │
                                   │  │ Storage (pluggable):  │ │
                                   │  │ workspaces, targets,   │ │
                                   │  │ tasks, secrets         │ │
                                   │  └───────────────────────┘ │
                                   └──────────────┬─────────────┘
                                                  │
                                   ┌──────────────┴──────────────┐
                                   ▼                              ▼
                        ┌────────────────────┐        ┌────────────────────┐
                        │ Local target        │        │ Remote target       │
                        │ (Loomux container)   │        │ (your PC, via SSH)  │
                        │  tmux pane: agent or  │        │  tmux pane: agent or│
                        │  bare shell           │        │  bare shell          │
                        └────────────────────┘        └────────────────────┘
                                   ▲                              ▲
                                   └──── ssh + tmux attach ───────┘
                                       (watch, or take over)
```

## Components

### 1. Targets (execution hosts)

A **target** is a host Loomux can run tmux sessions on. `local` is the
Loomux server's own container; any other target is reached remotely.

Interaction with a target goes through a `TargetExecutor` interface
(`RunTmuxCommand`, `CapturePane`, `KillSession`, etc.), so the mechanism for
reaching a target is swappable without touching anything upstream of it.

**v1 implementation: plain SSH.** Every operation shells out over SSH
(`ssh user@target 'tmux send-keys ...'`), using SSH connection multiplexing
to avoid per-call handshake overhead. This matches the existing convention
of reaching other hosts in this ecosystem (`ssh wyzer`, `ssh jet01`, etc.)
and requires nothing extra installed on the target beyond SSH access and
tmux.

**Everything Loomux runs on a target is POSIX sh, and only POSIX sh parses
it** (LOOM-90 review). A session's command is handed to tmux in argv form
(`sh -c <command>`), so tmux execs it rather than passing it to the
target's `default-shell`; and anything sent over SSH is a script on
stdin to `/bin/sh` — the remote user's login shell only ever parses the
word `/bin/sh`. Without this, a non-POSIX shell (fish, the default shell
on some of our machines) re-parsing POSIX-quoted text can end a quoted
string early and run what was meant to be data.

A future companion-daemon implementation (a small persistent process on the
remote target exposing a structured RPC surface instead of raw SSH+tmux
command strings) is a second implementation of the same interface, not a
redesign — deliberately deferred, since it adds real operational cost
(a service to install and keep alive on every target) that isn't justified
yet.

**Credentials on remote targets:** OAuth-authenticated agent CLIs (Claude
Code, etc.) are expected to already be logged in locally on machines you
own — Loomux doesn't push those sessions over SSH. API-key-style secrets are
injected as env vars via the SSH exec at launch time (see §7, Credential
Model).

**Target policy (LOOM-89).** Each target carries a policy saying what
Loomux may do there: `purpose` (personal or work), `allowed_agent_types`
(empty: all), `allow_provision`, `allow_shell` and `require_confirmation`.
The defaults allow everything, as before. The routing model is told each
policy, but the router enforces it after the decision and before
anything runs, whatever the model chose:
- A forbidden decision ends the turn with a plain refusal and no side
  effects: a new workspace where provisioning is off, a shell command
  where shell is off, or an agent type not on the list.
- On a require-confirmation target, new work waits for a "yes" in chat
  to the plan: a new workspace, a verbatim command, or an agent started
  in a workspace. The policy is checked again at the "yes" — of this and
  of every other offer (an agent install, a clone the user didn't name,
  a command not given verbatim), so a policy tightened after the offer
  still holds.
- Follow-up turns in the conversation's open pane there aren't asked
  again.

A shared work machine such as sc1 is meant to be `purpose: work`,
`allow_provision: false`, `require_confirmation: true`.

### 2. Workspace registry

A **workspace** is a directory (with an optional git remote) bound to
exactly one target, tracked in a storage-backend-agnostic registry.

Conceptual schema:

- `targets`: id, name, kind (`local`/`remote`), connection info (host, user,
  key reference), `workspace_root` (where dynamic workspaces go; absolute,
  clean, not `/`; empty = `$HOME/loomux-workspaces` on the target — LOOM-90)
- `workspaces`: id, name, path, target_id (FK), git remote (nullable),
  short domain tags + description (what the router matches requests
  against), capabilities (MCPs/tools available there), status
  (idle/active/provisioning/archived/failed — `failed` is a workspace whose
  provisioning failed: kept for inspection, never offered to the router
  again; LOOM-71), `status_reason` (why it's in that status — e.g. what
  made it `failed`; LOOM-77), `is_dynamic` (pre-registered vs.
  auto-provisioned on demand), last-used timestamp, `rolling_summary`
  (a single text field, *replaced* — not appended — after each completed
  task, so it stays lightweight by design and never grows into a log)
- `tasks`: id, workspace_id (FK), pane `kind` (`agent`/`shell`/`command`),
  agent_type (nullable for shell panes), tmux session/window name, status
  (running/awaiting-input/human-takeover/completed/failed), timestamps,
  the chat conversation it belongs to; for a `command` task, the command it
  ran verbatim and its exit code (LOOM-71); for a `failed` task,
  `failure_reason`, a stable `error_class` (`launch_failed`,
  `target_unreachable`, `send_failed`, `agent_exited`, `provision_failed`,
  `wait_failed`, `relay_failed`, `session_lost`, `internal`) and
  `output_tail` (bounded, credentials redacted) — LOOM-77
- `target_agents`: target_id (FK, cascade), agent_type, available, the
  absolute `path` the CLI resolved to and the `version` it reported
  (LOOM-79), checked_at — the last result of probing a target for an agent-type's CLI
  (LOOM-71, see §6 "Agent availability"). No row means "never checked",
  not "absent".

**Entity IDs are strings (UUIDs), not backend-native autoincrement
integers** — ratified during LOOM-3's implementation. Keeps IDs stable and
collision-free across a future second storage backend, whose native
autoincrement semantics would otherwise differ from the first backend's.
Every backend implementation must generate IDs this way to stay conformant
with the shared test suite (see Testing strategy below).

Workspaces are either **fixed** (pre-registered, e.g. mirroring today's
theWyseKube/jetone-infra/scds-infra/command-center split) or **dynamic**
(provisioned on demand — e.g. cloning a new repo).

**Provisioning runs nothing the router model wrote** (LOOM-90). The router
describes a new workspace only as structured data — a `name` (a slug,
`[a-z0-9][a-z0-9-]*`, ≤63), a `kind` (`empty` | `git_clone` |
`existing_dir`) and, for `git_clone`, a `git_remote` restricted to
`https://`, `ssh://` or scp-style `user@server:path` (never `ext::`,
`file://` or anything option-like: user and host must start with a letter
or digit, an scp-style path must not start with `-`). A remote the user
didn't type — the exact URL doesn't appear in their message, checked in
Go — is never cloned without confirmation: the reply shows the remote,
the target and the resulting path, and only a "yes" as the next message
(the same deterministic, one-shot mechanism as install offers) clones it
and carries the original request on. An agent started in a cloned
repository runs whatever its own config declares, so the repository is
the user's choice, not the router model's. Go validates it before anything is
written, then builds the provisioning script itself, every value quoted:
create/clone/adopt `<workspace_root>/<name>`, resolve it with symlinks
followed (`pwd -P`), refuse it unless it is still inside the resolved root
(so a symlink can't hand an agent `~/.ssh`), and print the resolved
directory — which becomes the workspace's path, so the agent starts in a
directory that exists and is confined. The script runs as a `command` task
(its exact text and exit code recorded), bounded at 10 minutes; any
failure leaves the workspace `failed` with a reason, its output redacted.
Arbitrary commands go only through `run_command` (§6), with its
verbatim-or-confirmed safety model.

**Deleting and repairing a workspace** (LOOM-70).
`DELETE /api/v1/workspaces/{id}` deletes the workspace and every task in it
(their per-turn transcripts with them) in one transaction, then kills those
tasks' tmux sessions. The chat transcript stays: messages lose only their
task link. The workspace's files on the target are never touched. It is
refused (409, with the reason) while a task there is mid-turn (cancel it or
let it finish) or taken over by a person (release it), while the workspace is still provisioning (the reaper fails
a stuck one, which can be deleted then), or while a credential is scoped to
it. Sessions it couldn't kill (target unreachable) are returned as
`sessions_not_killed`; the orphan sweep removes them later, since no task
owns them any more. `PATCH /api/v1/workspaces/{id}` with `{"status": ...}`
is the repair: `idle` puts a `failed` or `archived` workspace back in
service, clearing its `status_reason`, but only once its directory is
confirmed to exist on the target; `archived` takes an idle or failed one out
of the router's view while keeping its tasks and history. Nothing else is
settable there.

### 3. Pane kinds and agent lifecycle

Every tmux pane Loomux manages is one of two kinds:

- **`agent`** — runs a configured agent-type's CLI in its native interactive
  mode (not a headless/print-and-exit mode — see §5 for why).
- **`shell`** — a plain interactive shell, with no agent process. Used for
  workspace provisioning (cloning a repo, running setup scripts) and for
  giving a human a raw shell in a workspace on request. Completion for a
  Loomux-driven `shell` task is just script exit; for a human-requested one,
  it's the human closing it.
- **`command`** (LOOM-71) — a one-shot command run as the pane's own
  process: an agent CLI install, or (LOOM-72) a direct shell command. It
  runs under POSIX `sh -c`, never the target's default shell (tmux would
  otherwise hand it to e.g. fish, so the same command would mean different
  things on different targets). Its completion is the process exiting,
  never an idle heuristic; the command (as given) and its exit code are
  recorded on the task, and the pane is torn down once its output has been
  read.

**A pane outlives its process** (LOOM-71/LOOM-74). Every session is created
with tmux `remain-on-exit` set in the same tmux invocation as `new-session`,
so a command that exits — a finished provisioning script, an agent CLI that
isn't installed, a crash — leaves a dead pane whose exit status
(`#{pane_dead_status}`) and final output (including scrollback, where tmux
pushes a fast command's output) can still be read. Before this, such a
session vanished with its process and every later tmux call failed with
"can't find pane". Whoever owns the session still kills it: after a
completed task's grace period (step 3 below), and a failed task's dead pane
is left for inspection like any other failure.

Lifecycle for an `agent` task:

1. A chat message arrives. The router decides: answer directly (no agent
   needed), route to an existing workspace, or provision a new one.
2. If no task is currently running for that workspace + conversation, the
   orchestrator opens a tmux pane on that workspace's target and launches
   the configured agent CLI there, interactively. If one's already running,
   the message is sent into it as the next turn.
3. The orchestrator watches for a completion signal (§5). On completion, the
   router relays/summarizes the captured output back to chat, the
   workspace's rolling summary is updated (replaced, not appended), and —
   if the task itself (not just the turn) is finished — the task is
   completed and its pane is torn down 15 minutes later (LOOM-91), so a
   person can still attach and see what the agent did. A minute-by-minute
   finished-pane sweep does the teardown and marks the task reaped. The
   workspace registry row persists regardless; only the task-scoped pane
   goes away. Sessions are created 220x50 rather than tmux's detached
   default of 80x24, which wrapped and truncated an agent's answer.

   **What is relayed, and kept (LOOM-91).** A TierMarker agent's
   completion hook saves its CLI's payload beside the marker
   (`<marker>.reply`) before touching the marker. Claude Code's Stop hook
   hands it `last_assistant_message`, and Codex's notify
   `last-assistant-message`: the turn's whole final answer. The router
   relays that (bounded to 64 KiB, the start kept) rather than the
   pane's visible screen, which cut long answers to their last screenful.
   With no payload, the pane is captured as before. The files are read
   once, then removed, and cleared before each follow-up turn, so a turn
   that finished after it was given up on can't end or answer the next.
   Every finished turn is also stored (`task_turns`): the message sent,
   the agent's own message and the pane's last 3000 lines of scrollback,
   with credential values redacted. They are served at
   `GET /api/v1/tasks/{id}/transcript` after the task ends, a page at a
   time, latest first (`?limit=`, default 20, and `?before=<turn id>`),
   and kept for `LOOMUX_TURN_RETENTION` (30 days; `0` keeps them) — the
   reaper deletes older ones (LOOM-122).
4. At any point, attaching via `ssh` + `tmux -L loomux attach` to watch is
   free and has no side effects. Taking over (see §4) pauses automated dispatch for that
   task until released.

### 4. tmux interaction model

Every Loomux session lives on its own tmux server, `tmux -L loomux`
(LOOM-93), never the target user's default one. The user's tmux config
and plugins don't apply to agents, and Loomux sessions stay out of their
`tmux ls`. Attach-info returns the full `attach_command`. An hourly orphan
sweep lists `loomux-*` sessions on that socket per target. A session no
live task owns is logged, and killed once it is 24 h old, which leaves a
failed turn's pane a day for inspection. The sweep only ever touches that
socket and that prefix.

The orchestrator talks to tmux directly (`send-keys` / `capture-pane`),
shelling out locally or over SSH depending on the task's target. This is
deliberately the simplest viable mechanism — tmux already solves multi-client
attach, so a human watching a session is not a special case at all.

**Takeover / release.** Passive attach-to-watch never blocks or pauses
anything — tmux fans output out to every attached client for free. If a
human wants to actively type into a pane the orchestrator is also driving,
they signal intent explicitly (e.g. `loomux takeover <session>`), which
flips that task to `human-takeover` and pauses automated `send-keys` for it;
`loomux release` hands it back. This avoids keystroke collisions without
degrading the common case (just watching) at all.

### 5. Completion detection

Agents run in their **native interactive mode**, not a headless/print-mode
invocation — the initial design leaned toward headless mode purely because
process-exit is a trivially unambiguous completion signal, but that directly
undercuts the goal of being able to attach and interact with a running agent
live. Interactive mode is kept, and completion detection is solved
separately, in three tiers (strongest first):

1. **Native hooks**, where the agent-type supports them — e.g. Claude Code's
   `Stop`/`SubagentStop` hooks, wired to write a marker to a side-channel
   (file/FIFO/socket) the orchestrator watches. A real programmatic event,
   not an inference.
2. **Prompt-engineered self-report**, for any agent-type with tool-use/shell
   access but no native hook system — its system prompt/config instructs it
   to write a marker or hit a local socket when a turn is done. Covers
   nearly every realistic agent, since tool-use capability is close to a
   defining feature of "coding agent."
3. **Idle-time heuristic** — a true last resort, only for an agent-type with
   neither hooks nor tool-use capability to self-report at all. Explicitly
   the weakest signal in the system and not the default path.

**Process exit** (LOOM-71) runs alongside whichever tier applies: if the
pane's process exits before the tier signals, nothing ever will (a CLI
that isn't installed writes no marker), so the wait ends at once with the
exit status and final output. For a provisioning script exit 0 is success
and anything else a failure; for an interactive agent any exit mid-turn is
a failure, reported in the process's own words. A `command` task uses exit
as its *only* signal (`TierExit`).

Prompt-pattern matching against the rendered terminal UI (regex over
`capture-pane` output looking for a tool's own "ready" prompt) was
considered and rejected as a primary mechanism — live TUIs (spinners,
box-drawing redraws, ANSI escapes) make captured-text parsing fragile and it
silently breaks whenever a tool's rendering changes.

### 6. Router / orchestration model

The router is a single swappable configuration setting, not hardcoded to a
vendor. Its responsibilities:

- **Routing**: given the incoming chat message and the compact workspace
  registry (tags/description/capabilities — not full history), decide
  whether to answer directly, dispatch to an existing workspace, or
  provision a new one. This is the reason the registry carries short,
  structured metadata instead of Loomux stuffing full workspace history into
  every routing call — keeps the router's context small and cheap regardless
  of how many workspaces exist.
  The router is also given the registered targets (§1), since provisioning a
  workspace means choosing one — id, name and kind only. Hosts and
  credential references are never included: the routing prompt goes to a
  third-party model vendor. The chosen `target_id` is validated against
  that list, and re-resolved before any workspace row is written (LOOM-64).
  With no target registered, `provision_workspace` and `run_command` aren't
  offered at all, and a request that needs a machine gets a direct answer
  telling the user to register one (Targets page or `POST /api/v1/targets`)
  rather than a routing error (LOOM-68).
  Each target also carries its recorded agent availability (which agent
  CLIs were found there, which weren't, or "not checked yet"), so the
  router prefers an agent that is actually installed (LOOM-71).
- **Relay**: condense/summarize captured agent output into a chat-appropriate
  reply, and update the workspace's rolling summary.

- **Direct shell commands** (`run_command`, LOOM-72): a request like
  "run `hostname && uptime` on jet01" needs no AI agent. The router names
  a registered target and the command; Loomux runs it as a `command` task
  in that target's **shell workspace** (`shell@<target>`, created on first
  use in the login user's home directory, tagged `loomux:shell`, never
  offered back to the router) and replies with the command, its exit code
  and its output — relayed **verbatim**, never through the relay model.
  Safety model:
  - **Verbatim or confirmed.** The command runs at once only if the
    *whole message* is the user ordering exactly that command on exactly
    that target, in one of two shapes parsed in Go — ``run `<cmd>` on
    <target>`` or ``run on <target>:`` followed by a fenced block (optional
    "please", trailing punctuation, "run"/"on" in any case) — and the parsed
    command equals the router's `command` and the target name is exactly
    (case included — target names are case-sensitive) the name of its
    `target_id`. Anything else — a question about a command ("what does
    `rm -rf x` do?"), a command inside pasted logs or a README, a negation,
    a command the model wrote, completed or combined, a target the user
    didn't name — is shown back as the exact command *and target* and runs
    only if the conversation's very next message is an explicit
    confirmation (the same deterministic, one-shot, 15-minute, in-memory,
    model-free mechanism as install offers below).
  - The router prompt tells the model to copy commands character for
    character and never invent one; its `target_id` is validated against
    the registered targets.
  - **Output caps**: the last 200 lines / 16 KB, truncation marked. A
    command still running after 2 minutes is left running in its session
    (named in the reply, for a human to attach to), never killed.
  - **No secrets**: no credentials are injected into a command, and its
    output is scrubbed of every credential value in the vault (any scope)
    before it reaches chat; if the vault can't be read, the output is
    withheld rather than shown unscrubbed. The command itself is never
    logged — only its length.

**Agent availability and install offers (LOOM-71).** An agent-type may
declare the `Binary` its CLI runs as and an `Install` recipe (a fixed
install command plus the login step the CLI needs afterwards). Before a
workspace is provisioned for an agent, and before any fresh agent launch,
Loomux probes the target (`command -v <binary>` through the target's
executor, via POSIX `sh`) and records the result in `target_agents`.
The probe resolves an **absolute path** (LOOM-79), looking where a person
at that machine would find the CLI — their login shell's PATH, then the
non-interactive PATH, then common install dirs (`~/.local/bin`,
`~/.npm-global/bin`, `~/bin`, `/usr/local/bin`) — and records the
`--version` of exactly that path. The version check (§10 axis 3, now
configured for both real agent-types) and the launch then both run that
path, so a non-interactive SSH PATH that lacks `~/.local/bin`, or a second
install elsewhere, can't make the launch run a different binary than the
one probed and checked. The
recorded results can also be listed and refreshed on demand
(`GET /api/v1/targets/{id}/agents`, `POST .../agents/refresh`). A probe
that can't run (target unreachable) is an error, never "absent".

**Target health (LOOM-86).** Every target is also probed for health on a
timer (`LOOMUX_TARGET_PROBE_INTERVAL`, default 5m) and on demand
(`POST /api/v1/targets/{id}/probe`): reachability, latency, `tmux -V`,
and the space free on the workspace root's filesystem. A target that
answers has its agent CLIs re-probed in the same pass, including whether
each is signed in (`claude auth status`, `codex login status` —
classified as logged_in / logged_out / unknown; the output itself, which
can carry the account's email, is never stored). The latest result is
kept per target, shown on `GET /api/v1/targets` (`health`, with
`last_probed_at`), fed to the routing model (a target marked "unusable
right now" is avoided), and exported as `loomux_target_up` /
`loomux_target_disk_free_bytes`. A dispatch, command or provisioning
aimed at a target recorded unhealthy re-probes it first (unless that
record is under 30s old, which is trusted as it stands): recovered, the
work goes ahead; still broken, it fails at once with the probe's reason
(error class `target_unreachable`, or `target_unhealthy` for no tmux /
a nearly full disk) instead of a raw error minutes into the turn. A new
workspace also needs at least 1 GiB free on the root's filesystem.

If the turn would also have provisioned a workspace, the offer shows that
provisioning script in full too: a "yes" runs nothing the offer didn't
show.

When the agent is absent the turn ends with a reply instead of a failure —
and before any workspace row or session exists, so nothing is left stuck:
the exact install command and login step, and an invitation to reply
"yes". Safety model:

- The command that would run is the agent-type's configured recipe. The
  router model never supplies, edits or chooses it.
- The install runs only if the conversation's **very next** message is an
  explicit confirmation — a short fixed list of whole-message answers
  ("yes", "install it", "install codex", …) matched deterministically,
  without consulting the router model. Anything else, including "yes,
  but…", cancels the offer. Offers live in memory for at most 15 minutes
  and are one-shot; a restart forgets them (fails closed).
- A confirmed install runs as a tracked `command` task (in the workspace the
  original request targeted, provisioning it first if needed), its exit
  code and output relayed back, the target re-probed afterwards. Loomux
  never performs the login step itself.
- Output quoted back into chat or an error (an install's output, a failed
  provisioning's, an exited agent's last words) has credential values
  replaced with `[redacted]` — every vault value for install/provisioning
  output, the values injected for that workspace + agent for an agent's —
  *before* it is bounded (last 40 lines / 4 KB), so a secret straddling
  the cut can't survive as a tail.

Any number of agent-types can be registered; each supplies a launch command
template and a completion-detection adapter (§5, tier 1 or 2 config).

### 7. Credential model (hybrid)

Two different problems, two different solutions:

- **OAuth-based agent CLIs** (Claude Code's `claude login`, etc.) manage
  their own session/token lifecycle correctly already — Loomux doesn't
  reimplement this. It only ensures the pane's environment points at an
  already-authenticated config dir for that tool (shared `HOME`/config
  mount for local targets; expected to already exist on remote targets you
  own).
- **Everything else** (GitHub PATs, MCP server tokens, custom API keys)
  goes through a **credential vault Loomux owns**: encrypted at rest,
  scoped per agent-type/workspace, decrypted only at pane-launch time and
  injected as env vars. Same spirit as this repo's own SOPS + `bin/env.sh`
  pattern, but database-backed and access-scoped rather than one flat file
  everything reads from.

The router model's own credential (the LLM vendor API key(s) used for
routing/relay, §6) is a partial exception to both cases above: it's needed
before any workspace or agent-type is even chosen, so it fits neither the
OAuth-native-CLI case nor the workspace/agent-type-scoped vault. It's
configured once, server-wide, via environment variables at process start
(one set per configured tier — primary always, an optional escalation
tier for harder cases) — the same env-at-process-start convention as the
vault's own master key (§8), but deliberately outside the vault itself
since it isn't scoped to any workspace or agent-type.

### 8. Storage layer

Pluggable backends (not hardcoded to one database engine), with a real
migration story for moving between them:

- A storage interface covering the registry tables (§2) and the credential
  vault (§7), implemented against whichever backend is configured.
- Schema changes are ordered, numbered migrations tracked in a
  `schema_migrations` table, applied identically regardless of backend —
  switching backends means replaying the same migration set, not
  hand-building parity per engine (see §10, Versioning).

### 9. Auth model (clients)

Single-user for v1 — but real login-gated auth, not network-position trust.
Clients (web/Android/iOS) authenticate against the server; there is no
"it's just me so no auth" shortcut, since the mobile/web clients are
reachable outside the container's own trust boundary.

### 10. Versioning

Four independent axes:

1. **Server ↔ client API version.** Explicitly versioned (`/api/v1/...`),
   clients declare what they expect. A mismatch is a clear rejection or
   warning — not silent breakage. Matters even single-user, since mobile
   clients lag behind server updates (app store review delay).
2. **DB schema migrations.** Ordered, numbered, backend-agnostic (§8) —
   switching storage backends replays the same migration set.
3. **Agent-adapter versioning.** Each agent-type's adapter is coupled to
   that CLI's actual behavior; it declares a known-good version range for
   the underlying tool, checked at launch (e.g. `claude --version`). A
   drifted tool version fails loud at launch, not silently mid-task via a
   broken completion signal.
4. **Server release version** (semver), independent of the API version — a
   server patch release doesn't force an API version bump; only breaking
   API changes do.

## Error handling

- **Target unreachable** (SSH connection fails): task marked `failed`,
  workspace status reverts to idle, a clear error surfaces to chat — never
  a silent hang.
- **Agent CLI missing or exiting at once** (LOOM-71): probed for before
  launch and answered with an install offer (§6); if it still exits
  mid-turn, the task is `failed` and the error quotes its last output
  (e.g. `codex: command not found`, exit 127), credentials redacted.
- **Provisioning failure**: the workspace row is kept for inspection but
  marked `failed` — never left `active`/`provisioning` — with a
  `status_reason`, and excluded from routing. A provisioning script's
  non-zero exit is reported with its output. While provisioning runs the
  workspace stays `provisioning`: starting its session doesn't make it
  `active` (LOOM-77). The 10-minute provisioning bound only holds while
  the process that started provisioning is alive, so the idle reaper also
  fails any workspace still `provisioning` 15 minutes after it was created
  — left behind by a restart mid-provisioning, or written before
  provisioning was bounded — with a `status_reason` saying so; it runs
  once at startup as well as on each sweep (LOOM-60).
- **Every dispatch error after a task exists fails that task** with a
  reason and error class (LOOM-77) — a failed send, an abandoned or broken
  wait, a capture/relay failure —
  so nothing is left `running` with no explanation. A refusal because a
  human has taken the task over is not a failure. The one exception is a
  turn cut off by server shutdown: that leaves the task as it is, for
  startup reconciliation (LOOM-82) to pick up. At startup a job left
  mid-turn re-attaches to its agent if the session is still alive (the
  reply is delivered once the agent finishes), and a task whose session
  vanished during the downtime is failed with a reason.
- **Dispatch outlives the request** (LOOM-80,
  `docs/design/async-dispatch-design.md`): every `POST /dispatch` runs as a
  persisted dispatch job on a server-owned context, so a client going away
  mid-turn doesn't cancel it; the user message is stored at submit, the
  result lands on the job and in the conversation's history. Shutdown
  drains jobs, then marks the rest `interrupted`; startup marks any job a
  previous process left in flight `interrupted`. Failing a task returns
  an `active` workspace to `idle`; any other workspace status is kept.
- **Agent crash/hang** (bounded waits, LOOM-76): every agent turn is bounded
  per agent-type by `MaxTurnDuration` (default 1 h) and, for marker-tier
  agents, `NoProgressTimeout` (default 10 min of an unchanged pane with no
  completion signal — typically an agent blocked on an approval prompt).
  On either, the task is marked `failed` with error class `timeout` and a
  reason naming the agent, target and elapsed time, the workspace reverts
  to idle, and the error surfaced to chat names the tmux session to
  attach to. The pane is **not** auto-torn-down on failure — left alive
  for inspection via attach, since teardown is for successful/explicit
  completion, not silent cleanup of something that went wrong. A request
  cancelled by its caller is not a timeout. Provisioning (10 min) and
  direct commands (2 min, then left running) have their own bounds.
- **Agent stopped at a prompt** (LOOM-97): auto mode still asks a human
  about some things. Every progress check of a marker-tier agent's pane
  also reads it for a prompt (`agents.DetectPrompt`: a permission
  request, an AskUserQuestion, the folder-trust dialog, a sign-in
  screen). The same prompt on two consecutive checks ends the wait at
  once, rather than after the no-progress timeout. A sign-in screen
  fails the task with error class `login_required`, the pane kept so a
  human can attach and finish the login; Loomux never signs an agent in.
  Anything else makes the task `needs-attention`, with the prompt stored
  on it (`attention`: title, detail, question, options, cursor) and shown
  in chat. The conversation's next message is the answer, read
  deterministically, never by the routing model: a yes/approve word picks
  the first "Yes" option, a no/deny word the "No" option (or Escape), a
  number that option, and other words are typed into a question's "Type
  something" field or, for a permission, deny it and go to the agent as
  its next turn. Keys are arrow moves plus Enter, the same for Claude
  Code and Codex. A denied permission ends the agent's turn without its
  completion signal, so after a deny the turn also ends once the pane
  has been unchanged for 3 s. Then the turn carries on as any other:
  wait, relay, apply.
- **Flaky or saturated SSH** (LOOM-84): every ssh call sets
  `ServerAliveInterval=15`/`ServerAliveCountMax=3`, so a half-dead
  connection is dropped after ~45 s instead of hanging. Each operation
  has a deadline (30 s; 2 min for one-shot commands), and past it the
  operation fails as `ErrUnreachable`. At most 6 ssh operations run at
  once per target: they share one ControlMaster, and sshd's MaxSessions
  defaults to 10. Completion polling (marker, idle, exit) runs every 1 s,
  not every 250 ms. Not done yet: batching marker checks into one `ls`
  per target.
- **Router model unavailable/erroring**: an explicit "routing failed"
  message to the user, never a silently dropped message or a wild guess at
  a workspace.
- **Secrets decryption failure**: task fails fast at launch — never starts
  an agent with missing/broken credentials.
- **Concurrent takeover race** (human takeover request arrives just as the
  orchestrator sends input): last-writer-wins is acceptable for v1, given
  single-user scope — not worth over-engineering.
- **Storage unavailable**: server fails closed on new task dispatch rather
  than operating on stale/uncertain workspace state.

## Testing strategy

- `TargetExecutor` should be testable against a local/loopback SSH target,
  not only real remote boxes.
- Each completion-detection tier is independently testable: tier 1 via a
  fake hook script writing to a known file; tier 2 via a stub agent script
  that writes to a socket; tier 3 via a fake pane with scripted output
  delays.
- The storage layer should have one backend-conformance test suite run
  against every supported backend — adding a new backend means passing the
  existing suite, not hand-verifying it separately.
- The credential vault needs an explicit test that secrets never appear in
  captured pane output or the router's relayed chat text — a
  security-relevant test, not just a functional one.

## Deferred (separate future specs)

- Web, Android, iOS client UIs — downstream of the API this core defines,
  can't be meaningfully designed until it's stable.
- GitHub org / repo layout and provisioning mechanics.
- Specific router-model vendor selection.
- Multi-user support.
- Concrete agent-type adapters (Claude Code, Codex CLI, etc.) beyond the
  interface they implement.
