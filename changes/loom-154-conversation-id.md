### Fixed

- `POST /api/v1/dispatch` checks a client-supplied `conversation_id` and
  `workspace_hint` (LOOM-154): each must be 1–64 ASCII letters, digits,
  `-` or `_`, else `400 {error, code: "invalid_request"}`. Before, any
  string up to the body limit was accepted, including ones such as
  `a/b` that `GET /api/v1/conversations/{id}` can't fetch back.
  Server-minted ids (UUIDs) are unaffected.
