### Fixed

- The web client's server answers `404` for a missing file under
  `/assets/` or with a file extension, instead of `200` with
  `index.html` (LOOM-158). A browser still holding an old page now gets
  a clean chunk-load failure it can retry, not a MIME-type error.
  Client-side routes (`/conversations/…`) still get the app shell.
