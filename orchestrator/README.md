# orchestrator

Agent lifecycle, pane kinds, and the tmux interaction/takeover model. See
design spec §3, §4.

Covers `agent` vs `shell` pane kinds, the spin-up/teardown lifecycle
(task-scoped panes; the workspace registry entry persists across tasks),
`send-keys`/`capture-pane` driving, and the takeover/release handshake that
lets a human attach via SSH+tmux to watch for free or explicitly take over
without colliding with automated dispatch.

## Layout

- `orchestrator.go` — the `Orchestrator` struct, `New()`, the
  `CompletionDetector` and `ExecutorFactory` seams, and the error
  sentinels (`ErrHumanTakeover`, `ErrTaskInactive`).
- `lifecycle.go` — `Launch`, `SendMessage`, `WaitForCompletion`,
  `Complete`, `Fail`.
- `takeover.go` — `Takeover`, `Release`.
- `detectortest/` — `ManualDetector`, a `CompletionDetector` stand-in
  driven explicitly by test code (`Signal(taskID)`) rather than any real
  detection strategy. The real tiered strategy (native hooks/self-report/
  idle heuristic, design spec §5) is a separate concern — orchestrator
  code never assumes which tier is behind the seam.

`Complete` applies a caller-supplied summary to the workspace's rolling
summary verbatim — it never generates or condenses one itself (that's
the router's job). `Launch`'s `command` parameter is assumed to already
be a complete, resolved launch string — no agent-type→command-template
resolution or credential injection happens here.

Run `go test ./...` from the repo root to run the full suite, including
one integration test that drives the orchestrator against a real local
tmux session (`TestIntegration_RealLocalTmux`), not just the fake
in-memory executor the rest of this package's tests use.
