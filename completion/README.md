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
  *how* the agent was told to signal, not the detection code. Goes
  through `TargetExecutor.FileExists`/`RemoveFile` for both local and
  remote targets — one code path either way (LOOM-11), so it needs an
  `ExecutorFactory` the same way `IdleWatcher` already does.
- `idle.go` — `IdleWatcher`: tier 3, polling `TargetExecutor.CapturePane`
  and treating "no change for the configured idle timeout" as
  completion. Timing is abstracted behind a small `Clock`/`Ticker` pair
  so idle-duration-threshold logic is testable deterministically, with
  no real sleeping.

Marker detection works identically for local and remote targets —
`Detector.resolve` has no target-kind restriction; it only inspects
configuration. It also never probes reachability up front, so an
unreachable remote target surfaces as a plain error from `Wait` (same as
any other `TargetExecutor` operation), not a special-cased fallback to
idle.

Run `go test ./...` from the repo root to run the full suite, including
an integration test that drives the idle heuristic against a real local
tmux pane (`TestIntegration_IdleWatcherRealLocalTmux`), and marker tests
that exercise the real remote (SSH) path via `targets/sshtest` — not
just the scripted fake executor most of this package's tests use.
