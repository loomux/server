### Added

- Every `/api/v1` error body now carries a machine-readable `code` next
  to the human-readable `error`: `{error, code}`. The code defaults from
  the status (`invalid_request`, `unauthorized`, `not_found`,
  `conflict`, `rate_limited`, `internal`, ...); specific codes mark the
  cases a client can act on: `conversation_busy` (dispatch `409`, still
  with `dispatch_id`), `idempotency_conflict` (dispatch `422`) and
  `unsupported_api_version` (`404` outside `/api/v1`). Statuses and
  messages are unchanged. See `api/README.md`, "Errors".
