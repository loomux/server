### Changed

- Messages reach an agent as one bracketed paste (`tmux load-buffer` +
  `paste-buffer -p`, then Enter) instead of typed keys, so a multi-line
  message arrives whole (LOOM-111). Control characters other than
  newline and tab are dropped from it, so text can't end the paste early
  and press keys (tmux before 3.5 pastes an ESC verbatim).

### Added

- Error class `message_too_large`: a message over 32 KiB with its context
  is refused before anything is sent, and the agent's task is left as it
  was. Put the long part in a file in the workspace instead.
