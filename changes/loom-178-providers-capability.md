### Added

- Machines on demand (LOOM-178, the target-provider capability of
  `docs/design/target-providers.md`): a plugin that declares
  `targets.create` makes a machine per target, and Loomux registers it
  as an ordinary managed remote target with a Loomux key authorized on
  it and a host key Loomux generated and pinned before the machine
  existed. `POST /api/v1/targets` with a `plugin` object creates one;
  every target carries a `plugin` object (null for a registered host);
  `GET /api/v1/targets/{id}`; `POST /api/v1/targets/{id}/start`, `/stop`
  and `/recreate`; `DELETE` of a machine removes its workspaces, tasks,
  target-scoped credentials, the machine and its data. Scan, pin and
  migrate-ssh answer `409` for a machine. The reconcile loop resumes a
  machine left creating by a restart, makes a persistent one again if
  it vanished, marks an ephemeral one lost (archiving its workspaces),
  and destroys orphans after 24 h. Migrations `00029_environments` and
  `00030_credentials_target_scope`.
- Credentials may be scoped to a target (`target_id` on
  `POST /api/v1/credentials`): every agent on that machine gets them,
  below a workspace-scoped credential and above an agent-type one.
- Attach-info lists `attach_commands`: a plugin's ways in (kubectl,
  docker) for a machine it made, and the plain ssh form for every
  remote target.
- The routing model is told which targets are machines a plugin made
  (and which are ephemeral); dispatch to a stopped machine starts it
  first. `loomux_environments` is exported. The fake plugin, for tests,
  makes in-memory machines, and the conformance suite covers
  `targets.*`.
