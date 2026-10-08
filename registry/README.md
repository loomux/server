# registry

Workspace registry and pluggable storage layer. See design spec §2, §8.

Covers the `targets`/`workspaces`/`tasks` tables and the storage interface
that makes the underlying DB backend swappable, plus the migration story
for moving between backends. Also covers the credential vault's storage
(design spec §7, §8 groups the registry tables and the vault under one
storage interface) — see `credentials/` for the resolution logic built
on top of it.

## Layout

- `registry.go`, `credential.go`, `session.go`, `store.go`, `errors.go` —
  domain types and the `Store` interface. No backend-specific imports, so
  this stays the contract every backend implements. `Credential.Value` is
  plaintext at this level — encryption at rest is a backend
  implementation detail, the same way `Workspace.Tags` is plain
  `[]string` here despite being JSON-encoded in `sqlite`. `Session` backs
  client auth (`api/`, design spec §9) — only a hash of each bearer token
  is ever stored, never the raw token. `Task.ReapedAt` is set by the idle
  reaper (`orchestrator.Reaper`, LOOM-16) — purely informational, doesn't
  change `Task.Status` or gate anything by itself; see
  `orchestrator/README.md`.
- `storetest/` — the backend-conformance test suite (`storetest.Run`).
  Any backend implementation should pass this suite unmodified.
- `sqlite/` — the first `Store` implementation, backed by SQLite via the
  pure-Go `modernc.org/sqlite` driver (no cgo, so the server can still
  build as a single static binary). Migrations live in
  `sqlite/migrations/` (ordered, numbered `.sql` files, run via `goose` at
  store-open time, tracked in a `schema_migrations` table per the design
  spec) — this *is* design spec §10 axis 2 (DB schema migrations):
  ordered, numbered, and backend-agnostic by construction, since
  `storetest.Run`'s conformance suite is what any future second backend
  would have to pass regardless of how it replays the same migration
  set. Credential values are encrypted with AES-GCM (`crypto.go`) before
  they touch the database and decrypted on read, bound to their row id
  as additional data (LOOM-175; rows sealed before that are re-sealed by
  the first `Open` with the key); the master key is an
  optional `Open` option (`sqlite.WithMasterKey`, sourced via
  `sqlite.KeyFromEnv` from a base64-encoded 32-byte env var) rather than
  a required parameter, since most callers never touch credentials.

Run `go test ./...` from the repo root to run the full suite against the
SQLite backend.
