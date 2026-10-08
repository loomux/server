### Fixed

- A target deleted while the periodic health sweep is running is skipped
  quietly (a debug line) instead of logging an ERROR "target health
  probe failed … not found" (LOOM-174). `POST /api/v1/targets/{id}/probe`
  on a target deleted mid-probe answers `404` (no such target) instead
  of `500`.
