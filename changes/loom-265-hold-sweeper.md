### Added

- Holds on tasks now end by themselves (LOOM-265, the terminal client's S4). A hold sweeper runs every 5 s: a hold whose lease ran out without a heartbeat ends as `expired`, one whose session was logged out, revoked or has expired as `session_revoked`, and one whose task ended as `task_ended`. For the first two, the tmux clients attached to the task's session are detached (`tmux detach-client -s`, through the task's target) and then the task is handed back to automation, as a release does. This fails closed: if the clients can't be detached (the target doesn't answer), the hold and the task's `human_takeover` stay and the next sweep tries again. A `task_ended` hold detaches nobody.
- Restart grace: on start the sweeper first gives every active hold a full lease from then, so a restart or deploy longer than a lease doesn't end the holds of terminals still attached, which resume heartbeating once the API answers.
- `POST /api/v1/logout` and `DELETE /api/v1/sessions/{id}` end the session's holds at once, as `session_revoked`, alongside its streams and waits.
- `TargetExecutor.DetachClients`: detaches every client of a session, matched by its exact name, on the executor's tmux socket. No session or no client is nothing to do.
- `api/apitest`: `Server.SweepHolds` runs one sweep on the test server's clock, and a session's `Detaches` counts its clients being detached.

### Changed

- A forced takeover detaches the previous holder's tmux clients before it takes the task and answers, so the new holder attaches alone. If they can't be detached it answers `502` with code `detach_failed` and the previous holder keeps the task.
- Take back (`POST /api/v1/tasks/{id}/release` without `hold_id`) detaches the holder's tmux clients before handing the task back. If they can't be detached it answers `502` with code `detach_failed` and changes nothing. A release by `hold_id` detaches nobody.
