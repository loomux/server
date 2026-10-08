### Fixed

- `GET /api/v1/health` no longer shows an anonymous caller the database's
  error text: a failed component's `error` is a fixed `unavailable` (or
  `not configured` for the router model), and the cause is logged
  (LOOM-162). A database that can't be pinged now makes the status
  `unhealthy`, not `degraded` (still `503`).
- `GET /api/v1/health/deep` probes every target and the sidecar at once
  under one 10s deadline, instead of one target after another at up to
  10s each with no overall limit (LOOM-162).
