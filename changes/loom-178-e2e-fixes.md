### Fixed

- A machine target's `next_step` is null while its machine is being
  created, started or recreated, as the design says: there's nothing
  to do yet, and `plugin.status` says why it isn't ready. It used to
  ask for a connection test the whole time (LOOM-178).
- A machine that isn't ready by its create deadline says so ("the
  machine didn't become ready within 10m0s"), not the error of the
  store write the deadline interrupted (LOOM-178).
