### Added

- Watch holds (LOOM-271, the terminal client's S10). `POST /api/v1/tasks/{id}/takeover` now accepts `mode: "watch"`: a hold that only says who is attached to the task's pane read-only. The task's status isn't touched, so automation keeps typing and a running turn carries on. Any number of sessions may watch a task beside the one that drives it, so a watch is never `409` `task_held`. Nobody is detached when a watch hold is taken, released, expires or ends with its session. A session has one watch hold per task (asking again renews it), and `force` with `watch` is `400`. A watch is never handed a command that can type: until the read-only attach commands exist (LOOM-272), its `attach` has an empty `attach_command` and no `attach_commands`.
- Watch holds are shown beside the drive hold (LOOM-271). Each task of `GET /api/v1/conversations/{id}`, attach-info and a takeover's `attach` carry `watch_holds`: the task's active watch holds, oldest first, `[]` when nobody watches. `hold` stays the drive hold only. A row of `GET /api/v1/conversations` carries `watchers`, the number of watch holds on its tasks, left out when nobody watches. The stream's `task_hold` event now carries `watch_holds` too and is also sent when a watcher comes or goes; its `hold` and `ended_reason` still speak of the drive hold only.

### Changed

- `POST /api/v1/tasks/{id}/takeover` no longer answers `400` `unsupported_mode` for `mode: "watch"`: it takes a watch hold (LOOM-271). The hold in the answer always has the mode asked for.

### Fixed

- The hold sweeper ends a watch hold whose lease ran out even while its target doesn't answer: its end detaches nobody, so it needs nothing from the target. Before, a target found down in a pass put off every later hold on it, which once watch holds exist would have left them listed as watching until the target came back (LOOM-271).
