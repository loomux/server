### Changed

- Task statuses are snake_case in the API, like every other enum:
  `awaiting_input`, `needs_attention`, `human_takeover` (were
  `awaiting-input`, `needs-attention`, `human-takeover`), in the
  conversation list, a conversation's tasks and the stream's
  `task_update` (API v1 freeze review, item 9). The web client reads both
  spellings.
