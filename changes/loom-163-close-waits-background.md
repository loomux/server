### Fixed

- Shutting down no longer races its own background work (LOOM-163):
  startup task reconciliation and the orphan and finished-pane sweeps
  are cancelled and waited for before the database closes (no more
  "database is closed" errors), and a turn notification already on its
  way is delivered rather than lost.
