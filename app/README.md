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
  fail-fast behavior, not a startup requirement here), and the router
  model's own config via `router/llmrouter.ConfigFromEnv()`.
- `app.go` — `App`, `Build(cfg)`, `DefaultAgentTypes()`: the actual
  wiring order is storage → executor factory → completion detection →
  orchestrator → credential resolver → LLM-backed routing model →
  router. `DefaultAgentTypes()` is the production
  `router.AgentTypeRegistry` — currently just `"claude-code"` (tier-1/2
  marker completion, launch template `claude`) plus the `""`
  bookkeeping entry completion detection needs for shell-kind
  (provisioning) tasks — deliberately excluded from what's offered to
  the router model as a real agent-type choice.

## Testing

`app_test.go` runs a real end-to-end dispatch (real SQLite via a temp
file, real `llmrouter.Model` hitting an `httptest.Server` standing in
for the LLM vendor) through the `answer_directly` path, which needs no
real tmux/agent CLI. Run `go test ./app/...`.
