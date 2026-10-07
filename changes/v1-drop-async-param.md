### Removed

- `?async=true` on `POST /api/v1/dispatch` (API v1 freeze review, item
  4): dispatch is async by default and `Prefer: respond-async` asks for
  it explicitly; the parameter is now ignored. `?wait=true` still asks
  for a blocking answer.
