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
`file://` or anything option-like). Go validates it before anything is
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
  process: an agent CLI install, or (LOOM-72) a direct shell command. Its
  completion is the process exiting, never an idle heuristic; the command
  and its exit code are recorded on the task, and the pane is torn down once
  its output has been read.

**A pane outlives its process** (LOOM-71/LOOM-74). Every session is created
with tmux `remain-on-exit` set in the same tmux invocation as `new-session`,
so a command that exits — a finished provisioning script, an agent CLI that
isn't installed, a crash — leaves a dead pane whose exit status
(`#{pane_dead_status}`) and final output (including scrollback, where tmux
pushes a fast command's output) can still be read. Before this, such a
session vanished with its process and every later tmux call failed with
"can't find pane". Whoever owns the session still kills it: on completion
as before, and a failed task's dead pane is left for inspection like any
other failure.

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
   if the task itself (not just the turn) is finished — the pane is torn
   down. The workspace registry row persists regardless; only the
   task-scoped pane goes away.
4. At any point, attaching via `ssh` + `tmux attach` to watch is free and has
   no side effects. Taking over (see §4) pauses automated dispatch for that
   task until released.

### 4. tmux interaction model

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
  Each target also carries its recorded agent availability (which agent
  CLIs were found there, which weren't, or "not checked yet"), so the
  router prefers an agent that is actually installed (LOOM-71).
- **Relay**: condense/summarize captured agent output into a chat-appropriate
  reply, and update the workspace's rolling summary.

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
  `active` (LOOM-77).
- **Every dispatch error after a task exists fails that task** with a
  reason and error class (LOOM-77) — a failed send, an abandoned or broken
  wait (including the request being cancelled), a capture/relay failure —
  so nothing is left `running` with no explanation. A refusal because a
  human has taken the task over is not a failure. Failing a task returns
  an `active` workspace to `idle`; any other workspace status is kept.
- **Agent crash/hang**: if no completion signal arrives within a generous
  timeout (accounting for tier-3 idle fallback), the task is marked
  `failed` and surfaced to chat. The pane is **not** auto-torn-down on
  failure — left alive for inspection via attach, since teardown is for
  successful/explicit completion, not silent cleanup of something that
  went wrong.
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
