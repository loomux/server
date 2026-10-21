### Fixed

- A pane that exits (an agent CLI that isn't installed, a provisioning
  command that fails) no longer loses its last output, or its exit
  status, to tmux under load (LOOM-198). tmux 3.4 and 3.5 close a pane as
  soon as they reap its process, dropping what they hadn't read yet; the
  pane now waits, up to 5s, for tmux to show its exit marker first.
