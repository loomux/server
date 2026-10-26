### Changed

- A machine created from chat is logged in a conversation's audit trail (`GET /api/v1/conversations/{id}/events`)
  as kind `machine`, with its `target_id` and no `workspace_id`. Before, it was a `provision`, which web 0.3.11
  shows as "Set up a workspace". Deploy a web client with the `machine` kind (loomux/web LOOM-247) first; an older
  client leaves the step out of the turn rail and the Today weave (LOOM-247).

### Added

- A plugin size left out because its name is longer than 64 characters is logged as a warning ("plugin size left
  out: its name is longer than 64 characters", with the plugin, the name's first 64 characters and its length),
  once per plugin and name for each server process, so an operator sees why the size is missing (LOOM-247).
