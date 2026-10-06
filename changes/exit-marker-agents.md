### Fixed

- An agent CLI that exits (127 when it isn't installed) is reported at once:
  agent panes print their own exit status like command tasks do, and a
  pane that carries it no longer waits for tmux to record the status.
