### Fixed

- A task launched in the first moments after loomuxd starts (a workspace
  provisioned or a message sent right then, or a queued message a
  restart re-runs) is no longer failed with "its session was gone after
  loomuxd restarted" (LOOM-303). Startup reconciliation listed the
  running tasks in the background, after startup had finished, so it
  could read a new task's row before the task's tmux session existed and
  take it for one lost in the restart. The list is now read before
  anything can launch a task.
