### Security

- A secret an agent prints across a line wrap is redacted (LOOM-157):
  panes are captured with tmux's wrapped lines joined, and a vault value
  of at least twelve bytes broken up by whitespace (a TUI's own wrap,
  spaced groups) still matches, in the same longest-first pass as exact
  matches.
