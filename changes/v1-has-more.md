### Added

- `GET /api/v1/conversations` and `GET /api/v1/conversations/{id}` now
  carry `has_more`, so paging them later won't break a v1 client (API v1
  freeze review, item 17). Without paging parameters nothing else
  changes: every item comes back and `has_more` is `false`. Paging
  follows the task transcript's convention: the conversation's messages
  take `?limit=` (1 to 500) and `?before=<message id>`, returning the
  latest messages, oldest first, with `next_before` for the page before;
  the conversation list takes `?limit=`. A bad `limit` or an unknown
  `before` is `400`. See `api/README.md`, "Lists and paging".
