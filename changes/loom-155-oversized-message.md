### Fixed

- `POST /api/v1/dispatch` refuses a message over 33 KiB with `413
  {error, code: too_large}` at once (LOOM-155), instead of routing it
  (a model call) only for the router to refuse it as `message_too_large`.
