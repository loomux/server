### Added

- A dispatch's JSON (`GET /dispatches/{id}`, a conversation's dispatches)
  carries `confirmation_id`, the offer the message answered. A client that
  retries a failed Approve or Deny sends it again, so the server refuses
  the retry if that offer has closed instead of answering a newer one.
