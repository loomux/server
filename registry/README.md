# registry

Workspace registry and pluggable storage layer. See design spec §2, §8.

Covers the `targets`/`workspaces`/`tasks` tables and the storage interface
that makes the underlying DB backend swappable, plus the migration story
for moving between backends.

## Layout

- `registry.go`, `store.go`, `errors.go` — domain types and the `Store`
  interface. No backend-specific imports, so this stays the contract every
  backend implements.
- `storetest/` — the backend-conformance test suite (`storetest.Run`).
  Any backend implementation should pass this suite unmodified.
- `sqlite/` — the first `Store` implementation, backed by SQLite via the
  pure-Go `modernc.org/sqlite` driver (no cgo, so the server can still
  build as a single static binary). Migrations live in
  `sqlite/migrations/` (ordered, numbered `.sql` files, run via `goose` at
  store-open time, tracked in a `schema_migrations` table per the design
  spec).

Run `go test ./...` from the repo root to run the full suite against the
SQLite backend.
