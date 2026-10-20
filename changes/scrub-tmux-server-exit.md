### Fixed

- loomuxd no longer fails to start with "scrub tmux environment: …:
  exit status 1" when the local tmux server exits during the start-up
  scrub (its last session ended meanwhile). The scrub starts over, and
  a server that's gone has nothing left to scrub.
