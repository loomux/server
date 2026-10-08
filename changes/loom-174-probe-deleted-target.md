### Fixed

- A target deleted while the periodic health check runs no longer logs
  `ERROR target health probe failed ... not found` (or `does not
  exist`): the sweep skips it quietly, at debug level (LOOM-174).
