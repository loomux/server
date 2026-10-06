### Added

- Credential vault management (LOOM-134): `GET/POST /api/v1/credentials`,
  `PUT /api/v1/credentials/{id}/value`, `DELETE /api/v1/credentials/{id}`,
  all authenticated. Values are write-only, so no response carries one. The
  listing reads no ciphertext, so a vault whose master key no longer
  matches can still be cleaned up.

### Fixed

- Two unscoped credentials with the same name are now a conflict
  (migration 00019). The table's unique constraint treated their NULL
  workspace as distinct.
