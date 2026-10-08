### Added

- `POST /api/v1/login` returns a `device` token and accepts it back as an optional `device` field (LOOM-151). A login from a browser that has logged in before is throttled on its own backoff, so failed attempts by anyone else can no longer keep the owner locked out. The token is void once the password changes.

### Security

- The login backoff now admits one password check at a time (LOOM-142): attempts sent in parallel each got checked before any failure was recorded, so the backoff didn't slow a parallel guesser. Attempts arriving while one is checked get `429` with `Retry-After`.
