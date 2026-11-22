### Added

- Holds are shown where clients look (LOOM-267, the terminal client's S6). Each task of `GET /api/v1/conversations/{id}`, attach-info (`GET /api/v1/tasks/{id}/attach-info`) and a takeover's `attach` carry `hold`: the task's active drive hold, or `null`. A row of `GET /api/v1/conversations` carries `held_by {client_kind, client_label, since}` while one of its tasks is held, and `pending_confirmation_id` while the conversation waits on a yes or no to an offer (its newest one still pending and not past its deadline). Both are left out when there is none. The conversation stream sends `event: task_hold`, `data: {task_id, hold, ended_reason?}`, when a task is taken, taken from its holder (`ended_reason` `taken_over`) or given back (`hold` `null`, with why), but not when a hold is renewed.

### Changed

- A message sent to a conversation whose task a person holds now fails with the new `error_class` `held` instead of `internal`, and the dispatch's `error` names the holder ("being driven from the terminal on `laptop` since …"), so a client can tell it from a fault and show who has it (LOOM-267).
