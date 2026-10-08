### Changed

- An `/api/v1` path no route matches answers `404 {error, code:
  not_found}`, and a known path called with a method it doesn't take
  answers `405 {error, code: method_not_allowed}` with its `Allow`
  header (LOOM-161). Both were plain text, the only API errors outside
  the documented `{error, code}` body.
