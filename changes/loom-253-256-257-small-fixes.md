### Changed

- `PUT /api/v1/targets/{id}` no longer needs `name`, `kind`, `host` and
  `user` on every call (LOOM-256): an omitted one keeps what's stored, so
  a one-field change sends one field. A change to a target's name,
  permission mode or policy no longer turns it `ready: false`; a change
  to how it's reached (kind, host, user, port, key, proxy, workspace
  root, host key pin) still does.

### Fixed

- A Claude Code agent that stops on an API error from its model provider
  (`⎿ API Error: …`, a refusal included) fails the turn at once as
  `agent_api_error`, quoting the error, instead of after the 10-minute
  no-progress timeout (LOOM-253).
- A timed-out turn's error says why it timed out once, not twice
  (LOOM-257).
