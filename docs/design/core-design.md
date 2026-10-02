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
  key reference)
- `workspaces`: id, name, path, target_id (FK), git remote (nullable),
  short domain tags + description (what the router matches requests
  against), capabilities (MCPs/tools available there), status
  (idle/active/provisioning/archived), `is_dynamic` (pre-registered vs.
  auto-provisioned on demand), last-used timestamp, `rolling_summary`
  (a single text field, *replaced* — not appended — after each completed
  task, so it stays lightweight by design and never grows into a log)
- `tasks`: id, workspace_id (FK), pane `kind` (`agent`/`shell`), agent_type
  (nullable for shell panes), tmux session/window name, status
  (running/awaiting-input/human-takeover/completed/failed), timestamps,
  the chat conversation it belongs to

**Entity IDs are strings (UUIDs), not backend-native autoincrement
integers** — ratified during LOOM-3's implementation. Keeps IDs stable and
collision-free across a future second storage backend, whose native
autoincrement semantics would otherwise differ from the first backend's.
Every backend implementation must generate IDs this way to stay conformant
with the shared test suite (see Testing strategy below).

Workspaces are either **fixed** (pre-registered, e.g. mirroring today's
theWyseKube/jetone-infra/scds-infra/command-center split) or **dynamic**
(provisioned on demand — e.g. cloning a new repo — via a `shell`-kind pane
that runs the provisioning script and inserts the registry row on success).

### 3. Pane kinds and agent lifecycle

Every tmux pane Loomux manages is one of two kinds:

- **`agent`** — runs a configured agent-type's CLI in its native interactive
  mode (not a headless/print-and-exit mode — see §5 for why).
- **`shell`** — a plain interactive shell, with no agent process. Used for
  workspace provisioning (cloning a repo, running setup scripts) and for
  giving a human a raw shell in a workspace on request. Completion for a
  Loomux-driven `shell` task is just script exit; for a human-requested one,
  it's the human closing it.

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
  The router is also given the registered targets (§1) — id, name, kind and
  host only, never credential references — since provisioning a workspace
  means choosing a target; the chosen `target_id` is validated against that
  list, and re-resolved before any workspace row is written (LOOM-64).
- **Relay**: condense/summarize captured agent output into a chat-appropriate
  reply, and update the workspace's rolling summary.

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
