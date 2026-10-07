### Changed

- The API's enums are all snake_case (API v1 freeze review, item 9):
  task statuses `awaiting_input`, `needs_attention`, `human_takeover`
  (were `awaiting-input`, `needs-attention`, `human-takeover`) in the
  conversation list, a conversation's tasks and the stream's
  `task_update`; and a target's `permission_mode` `accept_edits` (was
  `accept-edits`, which is still accepted as input). Agent-type names
  such as `claude-code` are identifiers and keep their spelling. The web
  client reads both spellings.
