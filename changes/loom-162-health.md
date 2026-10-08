### Fixed

- `GET /api/v1/health` no longer shows anonymous callers the database's
  error text: a failed component's `error` is the fixed `unavailable`, and
  the detail stays in the authenticated `/health/deep` (LOOM-162). It now
  says `unhealthy`, not `degraded`, when the database is down.
- `GET /api/v1/health/deep` probes every target and the sidecar at once,
  under one 10-second deadline, instead of one after another at up to 10
  seconds each; a target still silent at the deadline is reported
  `timed out` (LOOM-162).
