### Added

- The API v1 contract test now also records, per route, the status
  codes it answers with, the query parameters and request headers it
  reads and the response headers it sets, read from the handler's
  source: one dropped from a route fails `TestAPIv1Contract` as a v1
  break, a new one fails until recorded with `-update`. See
  `docs/release/versioning.md`, "The API v1 contract".
