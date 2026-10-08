### Fixed

- A turn waiting for its conversation's previous turn now gives up when
  it is cancelled or times out (LOOM-165), instead of blocking until
  that turn finishes.
