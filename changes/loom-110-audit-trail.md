### Added

- A dispatch audit trail (LOOM-110): every routing decision (with the
  router model and tier that made it), command and provisioning run
  (redacted, with its target and how it ended), offer and answer, agent
  turn and dispatch outcome is kept in the database and served at
  `GET /api/v1/conversations/{id}/events`. Kept 90 days
  (`LOOMUX_EVENT_RETENTION`; `0` keeps it).
