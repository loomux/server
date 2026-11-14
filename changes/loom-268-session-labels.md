### Added

- A logged-in session can approve a session for another client by its token's hash: `POST /api/v1/sessions` `{token_hash, label?, client_kind?}` (LOOM-268, the terminal client's S7). This is the server side of the terminal's link flow: the terminal makes its own token, the browser approves `hex(sha256(token))`, and the token itself never leaves the terminal. It answers `201` with the new session, `409` if a session already has that hash, and `403` unless the caller's own session was made by a password login, so a linked session can't link another. A linked session expires and is revoked like any other.
- Sessions carry a `label` (the device, at most 64 characters) and a `client_kind` (`web`, `tui`, `app`, or empty), shown on each row of `GET /api/v1/sessions`. A password login's session is `web`. A hold on a task now takes its `client_kind` from the holder's session, and its `client_label` too when the takeover doesn't send one.

### Changed

- Migration `00037` adds `label`, `client_kind` and `origin` to `sessions`. Sessions that exist when it runs are marked as made by a login, so they keep working and can approve a link; their label and client kind stay empty.
