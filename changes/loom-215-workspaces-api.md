### Added

- `POST /api/v1/workspaces` provisions a workspace on a chosen target without a chat (LOOM-215). It takes `target_id` and `name`, plus optionally a kind, a `git_remote`, a description and tags, and answers `202` once the workspace exists (status `provisioning`). The answer carries a `Location` to follow and the conversation the provisioning run is recorded under. The run starts a stopped machine first, then ends the workspace `idle` or `failed` with its reason. The same request again returns that workspace (`200`), and a name another workspace has is `409 workspace_name_taken`.
- `GET /api/v1/workspaces/{id}` returns one workspace, for following a provisioning (LOOM-215).

### Fixed

- A provisioning workspace no longer shows `idle` without a path for a moment when its recipe exits; it stays `provisioning` until the path is recorded (LOOM-215).
