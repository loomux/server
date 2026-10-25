### Changed

- A size whose name is longer than 64 characters is no longer offered by `GET /api/v1/plugins/{id}/targets` or to the chat, nor planned. Before, it was listed truncated, and no request could create it. The chat's offer now shows a size's strings bounded as the API does (LOOM-216).

### Fixed

- With no machine plugin running, a routing model that proposes a machine anyway gets the "no machine plugin" answer, and an open task is left alone. Before, this cost a corrective retry and an escalation (LOOM-216).
- A plugin too old to answer `targets.describe` for a name is refused egress `trusted` with that reason. Before, the user was told the name isn't in `trusted_machines` (LOOM-216).
- A confirmed machine is created only as the offer showed it. If the plugin's offer changes between the confirmation's check and the create, nothing is made (LOOM-216).
- A yes the target's policy refuses afterwards records the refusal as the confirmation's `status_reason` (LOOM-216).
