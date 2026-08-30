# Loomux — server

Chat-driven interface to on-demand, tmux-backed coding agents, with a
lightweight swappable router model deciding routing/relay between the chat
and the agent fleet. A human can SSH into wherever an agent is running and
attach to its tmux session directly, without breaking the automated side.

**Status: pre-alpha — domain layer (registry, targets, orchestrator,
completion detection, credentials, router, LLM-backed routing model) is
wired into a runnable `cmd/loomuxd` process. No client-facing API/auth yet
(LOOM-9) — `loomuxd` only exposes a local single-message/stdin interface.**

## Design

The core design spec lives at [`docs/design/core-design.md`](docs/design/core-design.md).
It covers the workspace registry, execution targets (local/remote via SSH),
the tmux interaction and takeover model, tiered completion detection, the
hybrid credential model, pluggable storage, client auth, and versioning.

Explicitly deferred to their own future specs: the web/Android/iOS clients,
the specific router-model vendor, and concrete agent-type adapters beyond
the interface they implement.

## Layout

Each top-level directory corresponds to a component boundary from the
design spec — see each directory's own README for what belongs there and
which spec section it implements.

## Tracking

- Vikunja project `Loomux` (command-center instance)
- Plane project `LOOM` (command-center instance), epic "Loomux — Chat-Driven
  Multi-Agent Orchestration Platform"
