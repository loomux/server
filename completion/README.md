# completion

Tiered completion detection. See design spec §5.

Three tiers, strongest first: (1) native agent-CLI hooks wired to a
side-channel, (2) prompt-engineered self-report via a file/socket marker
for tool-use-capable agents without native hooks, (3) idle-time heuristic
as a last resort. Agents run in native interactive mode throughout — this
was a deliberate design correction from an earlier headless-mode direction
that would have blocked live SSH interaction with a running agent.

## Layout

- `completion.go` — `Tier`, `AgentConfig`, `Config` (the minimal
  per-agent-type tier/timeout config this package needs — deliberately
  not the full agent-type registry spec §6 describes), and `Detector`
  (the real `orchestrator.CompletionDetector` implementation), which
  resolves a task's tier and dispatches to the matching watcher.
- `marker.go` — `MarkerWatcher`: tiers 1 and 2 share one mechanism (watch
  a deterministic per-task marker file for existence) since the only
  difference between a native hook and a prompt-engineered self-report is
  *how* the agent was told to signal, not the detection code.
- `idle.go` — `IdleWatcher`: tier 3, polling `TargetExecutor.CapturePane`
  and treating "no change for the configured idle timeout" as
  completion. Timing is abstracted behind a small `Clock`/`Ticker` pair
  so idle-duration-threshold logic is testable deterministically, with
  no real sleeping.

**Known limitation:** marker-watching only works for tasks on a `local`
target — it observes the Loomux server's own filesystem, and there's no
mechanism yet to observe a marker written on a remote target's
filesystem. `Detector` falls back to the idle heuristic for a
marker-configured agent-type on a remote target, rather than hanging
forever waiting for a signal that can't arrive there.

Run `go test ./...` from the repo root to run the full suite, including
one integration test that drives the idle heuristic against a real local
tmux pane (`TestIntegration_IdleWatcherRealLocalTmux`), not just the
scripted fake executor the rest of this package's tests use.
