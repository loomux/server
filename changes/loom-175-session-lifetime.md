### Security

- A session has an absolute lifetime (LOOM-175): 90 days after login it ends however recently it was used, where before a token used at least once a month stayed valid for ever. `LOOMUX_SESSION_MAX_AGE` sets it (`0` for no limit). An expired session's requests are `401`, its open streams end at the next heartbeat, and `GET /api/v1/sessions` no longer lists it. Retention defaults for sessions, dispatches, messages and confirmations are proposed in `docs/design/retention-proposal.md`.
