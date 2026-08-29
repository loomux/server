# loomux-server — Loomux core (chat-driven multi-agent orchestration)

This repo is the implementation of **Loomux**: a chat-driven interface to on-demand,
tmux-backed coding agents, with a lightweight swappable router model deciding
routing/relay between the chat and the agent fleet. A human can SSH in and
attach to a running agent's tmux session directly, without breaking the
automated side.

## Design

The design spec lives at `docs/design/core-design.md` in this repo. It was
originally brainstormed and approved in **command-center**
(`docs/superpowers/specs/2026-08-29-loomux-core-design.md`, commit `1ba90c6`)
— that copy is the historical record of what was approved. **This repo's copy
is the working reference once implementation starts**: if building something
reveals the design needs to change, update this copy and send a `handoff`
(see below) summarizing the change so command-center's record can be updated
too. Don't silently diverge from the design without flagging it back.

Covers: workspace registry + pluggable storage, execution targets
(local/remote via SSH), pane kinds + tmux interaction/takeover, tiered
completion detection, the hybrid credential model, router-model integration,
client auth, and versioning. Explicitly **out of scope for this repo**: the
web/Android/iOS clients (their own future repos/specs) and a specific
router-model vendor choice.

## Status

**Pre-alpha.** Structural skeleton only as of 2026-08-29 — no code, no
language/module setup yet. See the top-level READMEs (`registry/`,
`targets/`, `orchestrator/`, `completion/`, `credentials/`, `router/`,
`api/`) for what belongs in each and which spec section it implements.

## Cross-workspace boundary (critical)

**This workspace must NEVER write to Vikunja, Plane, or Trilium directly.**
Only **command-center** (`~/git/command-center`) is authorized to
create/update/close items there — that's what keeps labels, dedup,
resolution-doc format, and Vikunja↔Plane reconciliation consistent. Do not
improvise REST/token access to those systems even if credentials are visible
in context.

**Mechanism: `agent-mailbox`.** When you finish (or get blocked on)
ticket-relevant work, send a `handoff` message instead of writing a file
into command-center's repo:

```bash
python3 ~/git/agent-mailbox/tools/mailctl.py send --from loomux-server --to command-center \
    --type handoff --title "short topic" --body-file /tmp/msg.md --tickets Loomux#N
```

Body: what you did, commits, verification evidence, blockers, and what
remains. Full protocol: `~/git/agent-mailbox/RULES.md`. **Check your inbox
at the start of every session:**

```bash
python3 ~/git/agent-mailbox/tools/mailctl.py inbox loomux-server
```

Process/archive anything pending before starting new work.

This work maps to Vikunja project **`Loomux`** (id `7`) and Plane project
**`Loomux`** (identifier `LOOM`), epic "Loomux — Chat-Driven Multi-Agent
Orchestration Platform" (sub-issues LOOM-2…LOOM-10).

## Guardrails

- Follow the design spec (`docs/design/core-design.md`) — it's the result of
  a full brainstorming pass with real tradeoffs already argued through (see
  its own text for the reasoning behind each decision, e.g. why agents run
  in native interactive mode rather than headless). Don't reopen settled
  decisions without a concrete reason surfaced during implementation.
- Standard engineering workflow applies once code starts (TDD, etc.) — no
  Loomux-specific process rules beyond what's already in the design spec.
