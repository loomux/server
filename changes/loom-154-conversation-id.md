### Fixed

- `POST /api/v1/dispatch` checks a client-supplied `conversation_id`: it
  must be a UUID or 1-64 letters, digits and dashes, else `400`
  (`invalid_request`). It was any string, such as `a/b` or 10 KB of text,
  though it is a path segment everywhere else in the API (LOOM-154).
  `workspace_hint` is capped at 255 characters the same way.
