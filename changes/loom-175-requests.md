### Fixed

- The `Authorization` scheme is case-insensitive (LOOM-175): `bearer <token>` is accepted like `Bearer <token>`, as RFC 9110 requires.
- A request body over 1 MiB is `413` however malformed it is (LOOM-175). Before, a body that wasn't valid JSON from its first bytes was a `400`, so an oversized login could get either; a `Content-Length` over the limit is now refused without reading the body.
- A JSON request body must be one JSON value (LOOM-175): anything but whitespace after it (a second object, stray characters) is a `400` `malformed request body` instead of being ignored.
