### Fixed

- A pane whose command exits the instant it starts (an agent CLI that
  isn't installed) is reported with its real exit status instead of -1
  (LOOM-181): tmux can miss that process's SIGCHLD and then never records
  the status on its own, so the server is now asked to reap while the
  status is pending. Output tmux reads late in that wait is kept too.
