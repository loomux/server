# app

Composition root. Wires the workspace registry, TargetExecutor,
orchestrator, completion detection, credential vault, and router
dispatch pipeline (design spec §2-§7) into a runnable `App` with a
single `Dispatch` operation.

The versioned HTTP/WS client API and login-gated auth (design spec §9,
§10 axis 1) are explicitly out of scope here (LOOM-9, not yet built) —
this package only proves the whole stack constructs and is
dispatchable. A later HTTP/WS/auth layer is expected to import this
package and wrap an `*App` rather than re-wire these pieces itself;
`cmd/loomuxd` is the current thin CLI entrypoint built on it.

## Layout

- `config.go` — `Config`, `LoadConfig()`: reads `LOOMUX_DB_PATH`
  (optional, defaults to `loomux.db`), `LOOMUX_MARKER_DIR` (optional),
  `LOOMUX_MASTER_KEY` (optional — the credential vault's AES-256 key;
  unset means credential operations fail per `registry/sqlite`'s own
  fail-fast behavior, not a startup requirement here), the router
  model's own config via `router/llmrouter.ConfigFromEnv()`, and the
  idle reaper's timing (`LOOMUX_REAP_IDLE_THRESHOLD` default 24h,
  `LOOMUX_REAP_INTERVAL` default 1h — design spec's continuation model,
  LOOM-16). A zero `ReapIdleThreshold`/`ReapInterval` on a `Config` built
  directly (not via `LoadConfig` — existing tests do this) isn't an
  error: `build` applies the same defaults, so callers don't need to set
  every field.
- `app.go` — `App`, `Build(cfg)`, `DefaultAgentTypes()`: the actual
  wiring order is storage → executor factory → completion detection →
  orchestrator → credential resolver → LLM-backed routing model →
  router → a background idle-reaper goroutine (`orchestrator.Reaper.Run`,
  started here and stopped by `App.Close`). `DefaultAgentTypes()` is the
  production `router.AgentTypeRegistry` — `"claude-code"` (launch
  template `claude`) and `"codex"` (launch template `codex`, LOOM-22 —
  proves the interface generalizes beyond one CLI), both declaring
  `Tier: TierMarker` symmetrically, plus the `""` bookkeeping entry
  completion detection needs for shell-kind (provisioning) tasks —
  deliberately excluded from what's offered to the router model as a
  real agent-type choice.

  **Known gap, affects both entries equally:** neither entry's
  `TierMarker` declaration is actually backed by per-task marker-path
  injection. `MarkerWatcher` expects a marker file keyed by the task's
  internal UUID, but `AgentType.LaunchTemplate` is a static literal
  string (`agenttype.go`'s own doc comment: no per-launch templating),
  and that UUID doesn't exist until `Orchestrator.Launch` mints it —
  after the launch command is already built. So today there's no
  mechanism telling a launched `claude` or `codex` process which marker
  file to touch; both CLIs do have a plausible native-hook or
  self-report mechanism to wire (Claude Code's `Stop` hook; Codex's own
  `Stop` hook or its simpler `notify` config key), but connecting either
  one to a real per-task path is unbuilt. Flagged rather than solved
  here — fixing it means giving `LaunchTemplate` real per-launch
  parameterization, a bigger design question than adding a second
  adapter answers on its own. `Build` wraps an
  unexported, parameterized `build(cfg, agentTypes)` — the public
  signature always uses `DefaultAgentTypes()`; the split exists purely
  so tests can wire a fast `TierIdle` agent type against a real tmux
  session without needing a real `claude` CLI installed.

## Testing

`app_test.go` runs a real end-to-end dispatch (real SQLite via a temp
file, real `llmrouter.Model` hitting an `httptest.Server` standing in
for the LLM vendor) through the `answer_directly` path, which needs no
real tmux/agent CLI. `continuation_test.go` goes further — a real local
tmux session carries two turns of a conversation (LOOM-13: the second
turn's message is sent into the same session, not a fresh one, until
the fake LLM server's second `Relay` call says `Done: true`) — using
`build` directly with a custom agent type, per the note above.
`reap_test.go` proves the background reaper started inside `build`
actually tears down a real, idle tmux session on its own — no further
`Dispatch` calls, just waiting — then that a follow-up `Dispatch` for the
same conversation transparently picks up with a fresh session (the
router-level fallback, LOOM-16). This test is what caught a real bug in
the reaper's own filtering logic (see `orchestrator/README.md`'s Reap
section) before it shipped. Run `go test ./app/...`.
