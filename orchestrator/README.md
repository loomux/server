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
  `Complete`, `Fail`, `Reap`.
- `takeover.go` — `Takeover`, `Release`.
- `reaper.go` — `Reaper`: periodically sweeps for `AwaitingInput` tasks
  idle past a threshold (design spec's continuation model, LOOM-13/16)
  and tears their sessions down via `Reap`. See its own doc comment for
  exactly what counts as "idle" and why `Running`/`HumanTakeover` tasks
  are deliberately never swept regardless of how stale they look.
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

## Reap vs. Fail/Complete

`Reap` tears down an idle task's session but — unlike `Fail`/`Complete` —
deliberately leaves `Status` and the workspace's `Status` unchanged;
`registry.Task.ReapedAt` is set purely for visibility. A reaped task is
still logically open as far as the conversation goes; only its session is
gone. `router.dispatchToAgent`'s stale-session fallback (LOOM-16) is what
actually transitions a reaped task to `Failed`, the moment a follow-up
message discovers the session missing and falls back to a fresh launch —
`Reap` itself doesn't need to know that will happen.

**A real bug this design caught, not just avoided by inspection:** the
first cut of `Reaper.isReapable` included `Running`, reasoning that both
`Running` and `AwaitingInput` mean "the conversation is still open." A
real end-to-end test (`app.TestBuild_IdleReaper_TearsDownRealSessionAutomatically`)
caught this immediately — the reaper tore down a session mid-turn, while
`Dispatch` was still blocked in `WaitForCompletion` waiting for it. `Running`
means a turn is actively in flight (however long it legitimately takes);
its `UpdatedAt` doesn't advance again until that turn's completion signal
fires, so it ages exactly like a genuinely idle task even though nothing
is idle at all. Only `AwaitingInput` — set precisely when a turn
concludes and the task is left open for a follow-up that hasn't arrived —
is safe to sweep.

Run `go test ./...` from the repo root to run the full suite, including
one integration test that drives the orchestrator against a real local
tmux session (`TestIntegration_RealLocalTmux`), not just the fake
in-memory executor the rest of this package's tests use.
