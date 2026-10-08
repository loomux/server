### Added

- `POST /api/v1/targets/{id}/migrate-ssh` (LOOM-138) moves a target
  reached through the mounted SSH config to a Loomux-managed key. It
  resolves the real host, port, user, key and proxy with `ssh -G`,
  imports the key and pins the known host key. It has a dry run, tests
  the result, and rolls back if the test fails. The machine needs no
  change.
