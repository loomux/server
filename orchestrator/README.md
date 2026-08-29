# orchestrator

Agent lifecycle, pane kinds, and the tmux interaction/takeover model. See
design spec §3, §4.

Covers `agent` vs `shell` pane kinds, the spin-up/teardown lifecycle
(task-scoped panes; the workspace registry entry persists across tasks),
`send-keys`/`capture-pane` driving, and the takeover/release handshake that
lets a human attach via SSH+tmux to watch for free or explicitly take over
without colliding with automated dispatch.
