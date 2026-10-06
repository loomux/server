### Fixed

- A message whose paste fails (its agent's pane already exited) no longer
  stays in a tmux buffer on the target, readable with `tmux show-buffer`:
  the buffer is deleted when the paste doesn't happen.
