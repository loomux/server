# Changelog

All notable changes to Loomux (loomux/server, and the loomux/web client it
pins) are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Loomux uses
[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html): see
`docs/release/versioning.md`. Every merge to main is a patch release
whose notes are on its GitHub release; this file has the curated
milestones.

## [Unreleased]

What's coming waits in [`changes/`](changes/), one file per pull
request, until the next milestone folds it in here.

## [0.4.0] - Unreleased

Everything merged since 0.3.0 (the patch releases from 0.3.1 on), with
web client 0.3.39. Loomux is still pre-alpha and single-user. The main
lines:

- **Machines on demand.** Target-provider plugins make a machine per
  target and register it as an ordinary SSH target: a Kubernetes plugin
  (a pod per machine, with a no-egress option offered only where it's
  proven), a Docker plugin (a container per machine on one Docker host),
  and `ghcr.io/loomux/agent`, the image they run. A machine can be asked
  for from chat, behind a confirmation.
- **Targets without hand-written SSH config.** Loomux generates and
  holds SSH keys, scans and pins host keys, tests a target step by step,
  and moves existing targets to its own keys.
- **Router models set in the app**, OpenAI-compatible or Anthropic's
  native API, with keys set, tested and rotated from Settings.
- **The API v1 freeze** (the renames and removals below) and a contract
  test that keeps `/api/v1` additions-only from here.
- **The server side of the terminal client** (loomux/tui): taking a task
  over while you're attached to its pane, handing it back, and linking a
  terminal to your login.
- A round of security hardening, much of it from an audit (LOOM-139 to
  LOOM-175).

### Before you upgrade

- 18 database migrations (00020 to 00037) run at the first start. Keep
  the pre-migration snapshot: the first start with `LOOMUX_MASTER_KEY`
  re-seals vault credentials and router keys, which an older release
  can't read, so a rollback means restoring that snapshot (router tiers
  saved in Settings can instead be saved again).
- The database runs in WAL mode (LOOM-248, #480). Keep its volume on a
  local or block filesystem (not NFS or SMB), and copy it with `sqlite3
  loomux.db ".backup FILE"`, or stopped and together with
  `loomux.db-wal`. An existing database is converted at the first start.
  See `docs/deploy/container.md`.
- Chat messages aren't routed without `LOOMUX_MASTER_KEY` (LOOM-251):
  the vault must be readable to redact them.
- Work targets (`purpose: work`) now default to relay `none`: the router
  models see nothing of their work, and a reply is the agent's own final
  message. Set the target's `relay` to `full` to keep the old behaviour.
- Local targets are off in the container image (LOOM-141). A bare binary
  keeps them on, but its local agents now get an allow-listed
  environment instead of loomuxd's (`PATH`, `HOME`, `SHELL`, `TERM`, the
  locale, `SSH_AUTH_SOCK`, proxy and CA settings); give them API keys
  and tokens through the vault.
- API clients: task statuses and `permission_mode` are snake_case,
  `attention.resets_at` is `attention.resets`,
  `confirmations[].workspace` is `workspace_name`, and some fields and
  one route are gone (see Changed and Removed). The bundled web client
  handles all of it.
- Old rows are deleted by default now: sessions 7 days after they
  expire, finished dispatches and audit events after 90 days, answered
  or expired confirmations after 30 (LOOM-110, LOOM-193; set a retention
  to `0` to keep them). A session also ends 90 days after login however
  recently it was used (`LOOMUX_SESSION_MAX_AGE`, LOOM-175).
- Refused at startup: a negative `LOOMUX_DISPATCH_MAX_DURATION`,
  `LOOMUX_DISPATCH_DRAIN`, `LOOMUX_REAP_IDLE_THRESHOLD` or retention,
  and an `openai` router tier on Anthropic's API host whose base URL
  isn't `https://api.anthropic.com/v1` (LOOM-187).
  `LOOMUX_REAP_IDLE_THRESHOLD=0` now turns idle reaping off; it used to
  mean the 24 h default.
- A remote target's `host` must fit a host-name grammar and its `user`
  must be a login name (LOOM-114, LOOM-175). An existing target whose
  host doesn't fit can't be updated until the host is corrected.
- The Kubernetes plugin's Role needs `list` on `networkpolicies` in the
  agents namespace for egress `none` (LOOM-201);
  `deploy/test/kind/10-rbac.yaml` shows the rule.

### Added

#### Machines on demand (target-provider plugins)

- Plugins (LOOM-178, `docs/deploy/plugins.md`): separate programs
  speaking `loomux-plugin/1` (JSON-RPC over stdio or a unix socket),
  bundled in the image, put in the plugin directory
  (`LOOMUX_PLUGIN_DIR`) or run as a sidecar. `/api/v1/plugins` lists,
  installs, configures, enables, disables, checks, upgrades and removes
  them; a configuration is validated against the plugin's schema, and
  secret settings are encrypted with `LOOMUX_MASTER_KEY` and never
  returned. `GET /api/v1/health/deep` reports them, and `loomux_plugins`
  and `loomux_plugin_calls_total` are exported.
- Machines (LOOM-178, `docs/design/target-providers.md`): a plugin that
  can create targets makes a machine per target, and Loomux registers it
  as a managed SSH target, with a Loomux key authorized on it and a host
  key Loomux generated and pinned before the machine existed. `POST
  /api/v1/targets` with a `plugin` object creates one; `start`, `stop`,
  `recreate` and `DELETE` manage it, and deleting it removes its
  workspaces, tasks, target-scoped credentials and data. A reconcile
  loop resumes a create a restart interrupted, makes a persistent
  machine again if it vanished, marks a lost ephemeral one, and destroys
  orphans after 24 h. The routing model knows which targets are
  machines, a dispatch to a stopped machine starts it first, and
  `loomux_environments` is exported.
- The Kubernetes plugin (LOOM-179, `plugins/kubernetes`): a
  restricted-compliant pod per machine in one namespace, with a Secret
  for sshd's files, a PersistentVolumeClaim for its data and a headless
  Service. Egress `none` is offered only where NetworkPolicy is proven
  enforced (see Security). Egress `trusted` (LOOM-226) lets the machines
  an admin names in `trusted_machines` reach the internet, the LAN and
  the tailnet, given the cluster's policy for
  `loomux.io/egress=trusted`; it is never the default. Sizes may request
  less than their limits (`cpu_request`, `memory_request`; LOOM-241):
  the pod is then Burstable, fits on busier nodes, and is among the
  first evicted under memory pressure. Bundled, and published as
  `ghcr.io/loomux/plugin-kubernetes` for the sidecar form.
- The Docker plugin (LOOM-180, `plugins/docker`): a hardened container
  per machine on one Docker host (no capabilities, no privilege
  escalation, read-only root, pids and memory limits), reached over SSH
  at a port published on the host. The plugin reaches the engine through
  its local socket or over SSH with its own key and a pinned host key,
  through a SOCKS5 proxy if needed, and never holds loomuxd's keys.
  `port_range` (LOOM-220) keeps machines' ports in a range a firewall
  can allow. Docker offers no egress choice: the LAN stays reachable.
  Bundled, and published as `ghcr.io/loomux/plugin-docker`.
- The agent image, `ghcr.io/loomux/agent` (LOOM-178,
  `docs/deploy/agent-image.md`): what runs in a machine. tmux, sshd and
  the agent CLIs (Claude Code, Codex, opencode; pinned, auto-updates
  off) on Node 22, as user `agent`, with `/data` for workspaces and
  home, reached over SSH on port 2222.
- A machine from chat (LOOM-216): while a machine plugin is running, the
  routing model can propose one (plugin, name, size, whether it keeps
  its data, egress). Nothing is created until the conversation's next
  message confirms the offer the card shows, and a yes creates nothing
  if the plugin's offer has changed since; a yes refused like that
  leaves the card `denied` with the reason in the confirmation's new
  `status_reason`. With no machine plugin, asking for a machine gets an
  answer on how to get one. The audit trail records the step as kind
  `machine` (LOOM-247).
- Idle machines can stop (LOOM-217): with `LOOMUX_MACHINE_IDLE_STOP` set
  (e.g. `4h`; off by default), a persistent machine nobody has used for
  that long is stopped, and the next request starts it again with its
  data. Ephemeral machines are never stopped this way.
- Plugins and machines declared in the deployment: `LOOMUX_PLUGINS_SEED`
  (LOOM-219), JSON or the path of a JSON file, installs and reconfigures
  the plugin instances it lists and never touches one made in the UI or
  API. Secrets are references to an environment variable or a file,
  never values. Its `machines` list (LOOM-236, #437) creates the named
  machines if they're missing and keeps their target options as
  declared; it never destroys a machine, and a change of size,
  persistence or egress is only logged with the API call to make.
  `LOOMUX_PLUGINS_SEED_DRY_RUN=true` only logs the plan.
- `GET /api/v1/plugins/{id}/targets` returns what a plugin instance
  offers for a new machine (LOOM-238): sizes, whether data is kept by
  default, egress options, image, quota and machine count. A target's
  `plugin.progress` is `image_pull` while its machine pulls the image
  (LOOM-239).
- Plugins can report that a machine changed on its own
  (`targets.changed`, LOOM-213), and the host reconciles it within a
  moment rather than at the next probe (`LOOMUX_TARGET_PROBE_INTERVAL`,
  5 minutes).
- A plugin below its `min_host_version` (or, from 1.0, on another major)
  is refused with the reason; one built for an older 0.Y installs with a
  warning (LOOM-194).
- Credentials can be scoped to a target (`target_id`): every agent on
  that machine gets them, below a workspace-scoped credential and above
  an agent-type one.
- Attach-info lists `attach_commands`: a plugin's ways in (`kubectl
  exec`, `docker exec`) and plain ssh for every remote target, each also
  as `argv` for a client to run without a shell (LOOM-269).

#### Targets and SSH

- Target onboarding through the API (LOOM-114): scan a target's host key
  without trusting it, pin a fingerprint from that scan (the target is
  then checked against its pin alone), unpin, and test. `POST
  /api/v1/targets/{id}/test` checks SSH and tmux and returns `steps`:
  which of connect, host key, authentication and tmux failed, and why.
  Targets take an `ssh_port`.
- Loomux-managed SSH keys (LOOM-138): `/api/v1/ssh-keys` generates
  ed25519 keys and shows their public parts, the `authorized_keys` line
  among them; private keys are encrypted with `LOOMUX_MASTER_KEY` and
  never returned. A target registered with `generate_ssh_key` or an
  `ssh_key_id` is reached with that key and no ssh_config, once its host
  key is pinned, through `LOOMUX_SSH_PROXY` (`socks5://host:port`)
  unless it sets `ssh_proxy: none`. Targets show `ssh_mode`, `ssh_key`,
  `ssh_proxy`, `ready` and `next_step`.
- `POST /api/v1/targets/{id}/migrate-ssh` (LOOM-138) moves a target from
  the mounted SSH config to a managed key: it resolves the real host,
  port, user, key and proxy with `ssh -G`, imports the key, pins the
  host key, tests the result and rolls back if the test fails. It has a
  dry run, and the machine needs no change.
- A per-target relay policy: `relay` (`full`, `last_message` or `none`)
  decides what the router models, which are third-party APIs, see of a
  target's work. Under `none` the agent's own message is the reply, its
  session stays open, and a notification carries only its kind and
  workspace. Responses show `relay_effective`.
- A target can keep its idle sessions (`keep_idle_sessions`, LOOM-227),
  for a hub machine whose conversation waits days for its next message.
- `GET /api/v1/targets` says whether this server allows `local` targets
  (`local_targets`, LOOM-183).

#### Workspaces

- `POST /api/v1/workspaces` provisions a workspace on a chosen target
  without a chat (LOOM-215), starting a stopped machine first; the same
  request again returns that workspace. `GET /api/v1/workspaces/{id}`
  follows it.
- Git worktree workspaces (LOOM-228): `worktree: {base, ref?, branch?}`
  gives a workspace its own checkout and branch from a repository
  already on the target. `DELETE
  /api/v1/workspaces/{id}?remove_worktree=true` removes the worktree
  (git refuses one with uncommitted or untracked files); the branch
  always stays.

#### Router

- Router settings in the app (LOOM-185,
  `docs/design/router-settings.md`): set, test, rotate and remove each
  tier's provider (`openai` or `anthropic`), base URL, model and API key
  under `/api/v1/settings/router` without editing secrets or
  redeploying. A stored tier overrides `LOOMUX_ROUTER_<TIER>_*`, which
  stays the bootstrap and the fallback, and every change is audited.
  `POST /api/v1/settings/router/{tier}/models` lists the provider's
  models for the Settings picker (LOOM-191).
- The router can run on Claude through Anthropic's native Messages API
  (LOOM-186): `LOOMUX_ROUTER_PRIMARY_PROVIDER=anthropic` (or
  `LOOMUX_ROUTER_ESCALATION_PROVIDER`), an API key and a model. The
  provider defaults to `openai`, so existing configurations are
  unchanged.

#### Terminal client (the server side of loomux/tui)

- Taking a task over (LOOM-263, LOOM-264, LOOM-265): `POST
  /api/v1/tasks/{id}/takeover` gives your session a hold on the task (a
  lease of 15 to 300 seconds, renewed by `/takeover/heartbeat`) with its
  attach-info, and moves the task to `human_takeover`, which pauses
  automation into its pane. `/release` hands it back; release without a
  `hold_id` is Take back, from any session. Another session gets `409
  task_held` unless it forces. A hold ends by itself when its lease runs
  out, its session is logged out or revoked, or its task ends: the
  attached tmux clients are detached first, and if they can't be,
  nothing changes and the next sweep tries again. A restart gives every
  hold a fresh lease. Ended holds stay as an audit trail (migration
  00036).
- Linking a terminal to your login (LOOM-268): a session made by a
  password login can approve another client's session by its token's
  hash (`POST /api/v1/sessions`), so the token never leaves the
  terminal. Sessions carry a `label` and a `client_kind` (`web`, `tui`,
  `app`).
- Not yet: watch-only holds (`mode: watch` answers `400
  unsupported_mode`), and holds shown on conversations and the stream.

#### API and operations

- A dispatch audit trail (LOOM-110): every routing decision (with the
  model and tier that made it), command and provisioning run (redacted),
  offer and answer, agent turn and outcome, at `GET
  /api/v1/conversations/{id}/events`. Kept 90 days
  (`LOOMUX_EVENT_RETENTION`; `0` keeps it).
- Every `/api/v1` error body carries a machine-readable `code` beside
  `error`, with specific codes for the cases a client can act on
  (`conversation_busy`, `idempotency_conflict`, `task_held`, …). An
  unknown path or method answers the same way (`not_found`,
  `method_not_allowed`). See `api/README.md`, "Errors".
- `has_more` on the conversation list and on a conversation, and
  `?limit=` / `?before=` paging for a conversation's messages, so paging
  later won't break a v1 client. See `api/README.md`, "Lists and
  paging".
- A retention sweep (LOOM-193), at startup and hourly: sessions 7 days
  after they expire, finished dispatches after 90 days, answered or
  expired confirmations and ended holds after 30.
  `LOOMUX_SESSION_RETENTION`, `LOOMUX_DISPATCH_RETENTION` and
  `LOOMUX_CONFIRMATION_RETENTION` set each (`0` keeps the rows). Queued
  or running work and pending confirmations are never deleted, and
  messages are always kept. It deletes in small batches and is counted
  in the `loomux_retention_*` metrics.
- Notifications over the rate limit are held and sent as one digest
  instead of being dropped, and needs-you notifications are never held
  or skipped (LOOM-249, LOOM-250, #479).
  `loomux_notifications_total{kind,outcome}` counts every outcome.
- Every published image carries OCI labels (version, commit, source) and
  a GitHub build-provenance attestation (LOOM-194;
  `docs/release/versioning.md`, "Images").
- `deploy/restore-drill.sh` runs the restore drill end to end and prints
  a drill-record row (LOOM-127).
- `loomux_target_op_seconds` has a 2 s bucket and an `op="poll"` series
  (LOOM-254).
- At startup the server warns about each task in `human_takeover` with
  no active hold (LOOM-263).

#### For contributors

- An API v1 contract test (`TestAPIv1Contract`,
  `api/testdata/api-v1-contract.txt`): every `/api/v1` route with its
  body fields, status codes, query parameters and headers. Removing or
  changing one fails as a v1 break; adding one fails until it's recorded
  with `-update`. See `docs/release/versioning.md`, "The API v1
  contract".
- `github.com/Loomux/server/api/apitest` (LOOM-270): runs a real
  `api.Server` over SQLite, with a stub dispatcher and a fake target
  executor, for another module's tests (loomux/tui's).
  `Server.SweepHolds` runs one hold sweep.
- `TargetExecutor.DetachClients` (LOOM-265). The fake plugin makes
  in-memory machines, and the plugin conformance suite covers
  `targets.*`.

### Changed

#### Chat, routing and relay

- Messages reach an agent as one bracketed paste, so a multi-line
  message arrives whole (LOOM-111). Control characters other than
  newline and tab are dropped from it.
- A bare "yes", "ok" or "go" that answers no pending offer no longer
  starts work on the routing model's reading of it: unless it continues
  the conversation's open task, or goes to a workspace the conversation
  already used while a task is open, Loomux asks what to do (#478).
- The relay model is told what a turn was for (the message, the
  workspace's summary before it, the agent type), so a terse answer is
  relayed as the answer to its question (LOOM-112). An unusable relay
  answer is retried once on the same tier before escalating (LOOM-186).
- Work targets (`purpose: work`) default to relay `none`; personal ones
  keep `full`.
- Without `LOOMUX_MASTER_KEY`, chat messages can't be routed (LOOM-251).

#### Targets

- `PUT /api/v1/targets/{id}` takes a partial update: an omitted field
  keeps what's stored, and changing a name, permission mode or policy no
  longer makes a target `ready: false` (LOOM-256).
- A host may be a fully qualified name with its trailing dot, or a
  link-local IPv6 address with its zone id (LOOM-188).
- Turns on one remote target share one completion poll a second
  (LOOM-254): at 10 sessions on one host, target-op p95 went from 1.55 s
  to 0.31 s in a loopback load test. An unchanged pane is captured less
  often (every 5 s, backing off to 15 s).
- A target's health `error` ends with the failure's class, e.g.
  `(auth_failed)`.
- A key made by `generate_ssh_key` is deleted with its target, or when
  the target moves to another key.
- `LOOMUX_REAP_IDLE_THRESHOLD=off` (or `0`) turns idle reaping off
  (LOOM-227).

#### Router

- A saved router key is bound to its tier, provider and base URL
  (LOOM-189).
- An `openai` tier on Anthropic's API host must use its
  OpenAI-compatible endpoint, `https://api.anthropic.com/v1`, and sends
  `max_completion_tokens` there (LOOM-187).
- `/api/v1/health` checks the router configuration in force, Settings
  included.

#### Plugins and images

- One release run publishes every image (LOOM-194): `server`,
  `plugin-kubernetes` and `plugin-docker` as `:X.Y.Z`, `:sha-<short>`
  and `:main`, pushed only once every build and test passed, and the
  GitHub release names each image's `tag@digest`. A version or sha tag
  is never re-pushed. The agent image is rebuilt only when
  `deploy/agent` changes; otherwise a release pins the current one.
- Plugin manifests carry the server release they were built with, and
  their default `agent_image` is the agent image that release pins, as
  `tag@digest` (LOOM-194).
- A bundled or sidecar plugin follows the server without an upgrade when
  its capabilities and configuration schema are unchanged; otherwise it
  waits in `error` for Upgrade. A setting still at the plugin's previous
  default follows the new default.
- A plugin size whose name is longer than 64 characters is left out,
  with a logged warning, instead of being offered truncated (LOOM-216,
  LOOM-247).

#### Terminal client

- Taking over a running task ends the wait on its turn at once
  (LOOM-266): the dispatch ends `failed` as `taken_over`, no keys reach
  the agent, no notification is sent, and what the agent finishes
  arrives as a late reply once the task is handed back.
- A forced takeover and Take back detach the previous holder's tmux
  clients first; if they can't, the request answers `502 detach_failed`
  and changes nothing.
- Migration 00037 adds `label`, `client_kind` and `origin` to sessions;
  sessions that exist when it runs count as password logins.

#### API v1 freeze

- Enums are snake_case: task statuses `awaiting_input`,
  `needs_attention` and `human_takeover`, and `permission_mode`
  `accept_edits` (`accept-edits` is still accepted as input). Agent-type
  names such as `claude-code` keep their spelling.
- A usage-limit attention's reset text is `attention.resets`, not
  `attention.resets_at`: it is the agent's own words, not a timestamp.
  Tasks stored before still read.
- A conversation's `confirmations[].workspace` is
  `confirmations[].workspace_name`.

#### Web client

- The image serves loomux/web 0.3.39 (`web-1035795`; 0.3.0 before): the
  finished redesign (the conversation screen, Today, Machines, Vault,
  Settings, the decision card and the Inbox, PWA polish and
  accessibility), the client side of the API v1 freeze, the Plugins page
  and create-machine flow (LOOM-214), the machine offer card (LOOM-247),
  router settings with a model picker (LOOM-185, LOOM-189, LOOM-191),
  the targets UI (LOOM-138), the brand identity (LOOM-176), and the
  audit fixes (LOOM-139 to LOOM-175, LOOM-183).

### Removed

- `POST /api/v1/targets/{id}/agents/refresh`: `POST
  /api/v1/targets/{id}/probe` re-probes the agent CLIs too.
- `?async=true` on `POST /api/v1/dispatch` is ignored: dispatch is async
  by default, and `Prefer: respond-async` asks for it explicitly.
  `?wait=true` still blocks.
- `capabilities` and `is_dynamic` on workspaces: they were always `[]`
  and `true`.
- `ssh_key_ref` on targets: nothing used it, and a request that sends it
  has it ignored. Migration 00023 clears the stored values.

### Fixed

#### Agents and tasks

- A pane whose command exits at once (an agent CLI that isn't installed)
  reports its real exit status (LOOM-181), and a pane that exits under
  load keeps its last output and exit status (LOOM-198).
- Codex: the "Update available" startup menu, whose option runs `curl …
  | sh` on the target, no longer stops a task; every launch passes `-c
  check_for_update_on_startup=false` (LOOM-260, #487). A task already at
  Codex 0.163's update prompt shows as needing you (#482).
- A Claude Code agent stopped by an API error from its model provider
  fails the turn at once as `agent_api_error`, quoting the error,
  instead of after the 10-minute no-progress timeout (LOOM-253). A
  timed-out turn's error gives its reason once (LOOM-257).
- A task whose status changed during a turn (cancelled, or taken over)
  is no longer overwritten when the turn ends: status writes after a
  wait are compare-and-set (LOOM-262). A cancel that loses such a race
  answers `409`, not `500`.
- A task launched in the first moments after loomuxd starts is no longer
  failed as lost in the restart (LOOM-303).
- A message whose paste fails no longer stays in a tmux buffer on the
  target.
- loomuxd no longer fails to start when the local tmux server exits
  during the start-up environment scrub (#384).

#### Chat and dispatch

- An agent gets the task, without the routing instructions around it
  ("Use Claude Code in workspace api-server. …"); the conversation keeps
  the message as typed (LOOM-258).
- The routing model no longer asks for a "yes" itself, copying Loomux's
  offer text with no offer behind it (LOOM-255). A question asking for
  information stays.
- A conversation stream reports a turn that starts and ends right after
  the client connects (LOOM-137), and polls only what's new instead of
  rereading the tasks and the whole conversation every 500 ms
  (LOOM-145).
- Retrying a new conversation's first message with the same
  `Idempotency-Key` returns the original dispatch and its conversation
  (LOOM-140).
- A dispatch whose status couldn't be recorded no longer leaves its
  conversation busy until a restart (LOOM-146).
- A turn waiting for the previous one gives up when it's cancelled or
  times out (LOOM-165).
- `POST /api/v1/dispatch` checks `conversation_id` and `workspace_hint`
  (1 to 64 letters, digits, `-` or `_`; LOOM-154), and refuses a message
  over 36 KiB with `413` before any model call (LOOM-155). A message
  over 32 KiB with its context fails as `message_too_large` before
  anything is sent to the agent.
- With no machine plugin running, a proposed machine gets the "no
  machine plugin" answer without a retry and an escalation (LOOM-216).

#### Server and API

- Concurrent work no longer fails with `database is locked`: SQLite runs
  in WAL mode with IMMEDIATE transactions (LOOM-248, #480).
- Shutdown drains as intended (LOOM-147, LOOM-163): requests in flight
  get their full answer, streams end as shutdown begins, and background
  work is cancelled and waited for before the database closes.
- A missing static file (under `/assets/`, or ending in `.js` and the
  like) is a `404`, not the app shell, so a browser holding an old
  bundle sees a chunk load it can retry (LOOM-158).
- Health (LOOM-162): `/api/v1/health` no longer shows anonymous callers
  the database's error text, and says `unhealthy` when the database is
  down. `/api/v1/health/deep` probes every target at once under one
  10-second deadline.
- Requests (LOOM-175): the `Bearer` scheme is case-insensitive, a body
  over 1 MiB is `413` however malformed, and a JSON body must be exactly
  one value.
- A target deleted during a health sweep is skipped quietly, and probing
  one deleted mid-probe is `404`, not `500` (LOOM-174).
- Creating or updating a target that names a missing SSH key is a `400`
  saying so.

#### Targets and SSH

- The SSH ControlPath escapes `%`, so a config-mode target whose host
  carries a zone id connects.
- Pinned known_hosts files, managed SSH (key agents, keys, proxy), the
  local-target switch and the tmux socket belong to each server
  instance, not process-wide settings the last start overwrote
  (LOOM-197, LOOM-199, LOOM-202).
- A Docker or Kubernetes machine's attach command names the instance's
  tmux socket instead of always `loomux` (LOOM-210).

#### Router

- The primary circuit breaker no longer counts a late failure from a
  replaced primary against the new one.

#### Plugins and machines

- A sidecar plugin whose socket isn't there yet at start is retried for
  up to 10 minutes instead of being left in `error` (#465).
- A plugin upgrade no longer hangs when the new version's check reports
  a problem, and one that can't be stored leaves the old version running
  (LOOM-245).
- A slow first image pull is progress, not a failure (LOOM-203,
  LOOM-221, LOOM-222, LOOM-242): a machine's 10-minute create, start and
  recreate deadline, a dispatch's 5-minute wait for the machine, and an
  API provisioning all stand still while the plugin reports
  `image_pull`, for up to 30 minutes more. A machine past its deadline
  says so, instead of staying `starting` or showing the error of an
  interrupted write.
- A plugin's `max_environments` can't be exceeded by concurrent creates,
  from one server process or several (LOOM-201, LOOM-244).
- A machine target's `next_step` is null while the machine is being
  made, and a provisioning workspace stays `provisioning` until its path
  is recorded (LOOM-215). A confirmed machine is created only as offered
  (LOOM-216). `GET /api/v1/plugins/{id}/targets` passes on `cpu_request`
  and `memory_request` (LOOM-241).
- The Kubernetes network-isolation canary retries through a policy
  engine's start-up gap, rides out a transient API error (etcd "leader
  changed") and reports one that lasts the whole run, allows for a slow
  image pull (LOOM-178), and is deleted once its verdict is recorded
  (LOOM-212). The check warns `service_missing` when reading the
  headless Service fails (LOOM-246).
- A plugin too old to answer `targets.describe` for a name is refused
  egress `trusted` with that reason (LOOM-216).
- Every tag of a published image names the pushed, attested digest, so
  `gh attestation verify` works by tag (LOOM-194).
- A plugin's `targets.changed` notification no longer outlives the
  server's shutdown (LOOM-196).

#### For contributors

- Tests run tmux on sockets of their own and remove them afterwards
  (LOOM-210).
- The README banner's lead-in joins the l as one stroke (LOOM-300).

### Security

- Built with Go 1.27.4 and `golang.org/x/net` v0.60.0 (0.3.0 had Go
  1.27.0 and x/net v0.57.0), for the `net/http`, HTTP/2, `html/template`
  and `crypto/x509` advisories govulncheck reports, GO-2026-6809 and
  GO-2026-6859 among them (LOOM-302).
- A target's `user` or `host` can no longer be read by ssh as an option
  (a value like `-oProxyCommand=…` ran a command inside the Loomux
  server): such values are refused, and every ssh call puts `--` before
  `user@host`, which also covers targets stored earlier (LOOM-114).
  Hosts are checked against a host-name grammar (LOOM-175).
- Local targets are off in the container image (LOOM-141): an agent on
  one runs as loomuxd's own user, and so could read the database, the
  vault key and the SSH keys. Where they're on, local agents get an
  allow-listed environment, without `LOOMUX_*` secrets.
- Login (LOOM-142, LOOM-151): the backoff checks one password at a time,
  so parallel guesses are slowed too, and a browser that has logged in
  before gets a `device` token and a backoff of its own, so failed
  attempts by anyone else can't lock the owner out.
- Sessions end 90 days after login (`LOOMUX_SESSION_MAX_AGE`, LOOM-175).
  Logging out or revoking a device ends its open streams, blocking
  dispatch waits and task holds at once (LOOM-144, LOOM-182, LOOM-265).
- Browser hardening headers (LOOM-143): a Content-Security-Policy (the
  web client runs only its own scripts, connects only to this server,
  can't be framed, and loads images only from this server, so a reply
  can't beacon out), `X-Frame-Options: DENY`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on
  the API, and HSTS over HTTPS.
- Redaction: vault values are taken out of what the routing model gets
  and of the earlier-conversation note a new agent gets (LOOM-251); the
  longest values go first, so one credential that is part of another
  can't leave the longer one's rest behind (LOOM-156); a secret split by
  a line wrap or whitespace is caught (LOOM-157); overlapping matches
  are redacted as their union (LOOM-184); and credentials in URLs are
  redacted.
- Vault credentials are bound to their row and scope (LOOM-175,
  LOOM-192), and router keys to their tier, provider and base URL
  (LOOM-185, LOOM-189), so a ciphertext moved within the database no
  longer decrypts there. Router keys set in Settings are write-only (a
  fingerprint and the last four characters are shown), are never sent to
  a new provider or base URL without being entered again, and are
  scrubbed from agent output; stored base URLs must be https (http only
  to loopback).
- Provisioning: a repository is cloned without asking only when your
  message names that exact repository (LOOM-152), and a cloned
  repository's output can no longer choose where its agent starts
  (LOOM-153).
- SSH: a host-key scan can be pinned only for the address it was read
  from (LOOM-164), and the ControlMaster socket directory is checked and
  made 0700 before every ssh run, so another local user can't plant a
  socket there (LOOM-166).
- A plugin from the plugin directory runs only when its executable is
  vouched for (LOOM-218): by its sha256 in `LOOMUX_PLUGIN_DIGESTS`, or
  by a Sigstore bundle beside it with SLSA build provenance from a
  workflow in `LOOMUX_PLUGIN_SIGNERS`.
  `LOOMUX_PLUGIN_ALLOW_UNSIGNED=true` is for local development. Bundled
  and sidecar plugins are unaffected.
- Kubernetes egress `none` is certified only while it holds (LOOM-201,
  LOOM-208, LOOM-211). Every NetworkPolicy that may select a confined
  machine must allow only kube-dns, one must isolate it for egress, and
  a canary pinned to the machine's own node must have found
  NetworkPolicy enforced there within the last 10 minutes. The policies
  are read again every minute. If they stop confining a running machine,
  or a canary gets out on its node, the machine turns `error` (its pod
  is left running, and nothing more is dispatched to it) and `none` is
  withdrawn until a check passes again. After a plugin restart, `none`
  machines are `starting` until their nodes are checked again. Canaries
  need room in the agents namespace's pod quota: allow
  `max_environments` + 2 × (nodes + 1) pods, plus (nodes + 1) for each
  further restart within 4 minutes. What `none` still trusts is in the
  plugin's README.

## [0.3.0] - 2026-10-06

Ships loomux/web 0.3.0, the test-and-contract milestone: an end-to-end
suite against this server's image, a typed API client, the UI inventory
for the redesign, and the fixes those turned up. Server changes are small.

### Added

- A dispatch's JSON (`GET /dispatches/{id}`, a conversation's dispatches)
  carries `confirmation_id`, the offer the message answered. A client that
  retries a failed Approve or Deny sends it again, so the server refuses
  the retry if that offer has closed instead of answering a newer one.

### Changed

- The image serves loomux/web 0.3.0 (pinned in `deploy/web-ref`).

### Fixed

- An agent CLI that exits (127 when it isn't installed) is reported at once:
  agent panes print their own exit status like command tasks do, and a
  pane that carries it no longer waits for tmux to record the status.
- The image build pages through every loomux/web tag to name the web
  version, so a pinned release's `v*` tag can't fall off the first page.

## [0.2.0] - 2026-10-06

The first MINOR after the base: everything merged since 0.1.0 (the 0.1.x
patch releases), with web client 0.2.0. Loomux stays on 0.x
until API v1 is declared stable, which will be 1.0.0.

### Added

- Credential vault management (LOOM-134): `GET/POST /api/v1/credentials`,
  `PUT /api/v1/credentials/{id}/value`, `DELETE /api/v1/credentials/{id}`,
  all authenticated, and a Credentials page in the web client. Values
  are write-only: no response carries one. The listing reads no
  ciphertext, so a vault whose master key no longer matches can still be
  cleaned up.
- In-place web client updates, verified (LOOM-118): with
  `LOOMUX_WEB_UPDATES=attested` the server installs the newest bundle
  loomux/web's CI built on main, after checking its GitHub build
  provenance with sigstore-go (signing workflow, source repository, ref
  and commit, subject digest). `off` serves only the image's bundle;
  `pinned` only what loomux/server main pins.
- Agent-state detection (LOOM-109): an agent at its usage limit fails
  the turn as `agent_rate_limited`, saying when the limit resets
  (Claude Code and Codex); one that keeps compacting its context (3
  times in 30 minutes of one turn) fails as `compaction_loop` and is
  interrupted, its pane kept.
- `LOOMUX_TMUX_SOCKET` names the tmux socket an instance runs its sessions
  on (default `loomux`), so a test and a production instance can drive the
  same targets without seeing, or sweeping, each other's sessions.
- Install guide and operations page (LOOM-128), a non-destructive
  restore drill with its first record (LOOM-127), and a test that a
  v0.1.0 database upgrades and still reads back (LOOM-126).

### Changed

- Router resilience (LOOM-107): a routing answer that names an unknown
  workspace, target or agent (or isn't a usable tool call) gets one
  retry on the same model, told what was wrong, before escalating. When
  the primary model fails 3 times in a row it's skipped for 2 minutes
  and messages go straight to the escalation model; after that one call
  tries it again. New metrics `loomux_router_retries_total` and
  `loomux_router_primary_breaker_open`. The routing model sees at most
  25 workspaces per message: the most recently used, plus the hinted,
  last and open-task workspaces and any the message names.
- A target that can't be reached over SSH says why (LOOM-85): host key
  changed or unknown, key refused, DNS, SOCKS proxy down, connection
  refused, host unreachable, shared-connection session refused, or
  timeout, each with a hint. The class is in the error and in
  `loomux_target_op_errors_total{reason="unreachable_<class>"}`; ssh's
  banner lines no longer replace the real error.
- Workspace names may contain underscores (LOOM-130).
- The image's web client is web 0.2.0 (`web-7da25cc`): the Credentials
  page, usage-limit wording, the chat scrolling to the newest message,
  clearer cancel and update wording, names instead of ids for new
  workspaces and targets; its bundles carry build-provenance attestations.
- Releases: every merge to main is a patch pre-release, MINOR by hand
  per milestone (LOOM-129); changelog entries are files in `changes/`.

### Fixed

- A command task reports its own exit status, so a provisioning step
  that exited 0 is no longer misreported as "status -1" when tmux records
  the status late (LOOM-136); a pane's end reads "exit N", "killed by
  signal N" or that tmux never recorded one.
- A new tmux session that raced the server's shutdown is retried once.
- A dispatch job whose agent isn't signed in records `login_required`
  (it recorded `internal`), so the web client shows its sign-in hint.
- Two unscoped credentials with the same name are a conflict (migration
  00019); the unique constraint treated their NULL workspace as distinct.

## [0.1.0] - 2026-10-05

The first release, collecting everything built so far, with web client
0.1.0. From here on, every merge to main is a patch release with its
own notes.

### Added
- Chat-driven orchestration: one-password login, conversations, and a
  router model that answers directly, runs a shell command on a target,
  or hands the work to a coding agent.
- Agents in their native interactive mode in tmux (Claude Code, Codex):
  completion hooks, launch profiles, version checks, absolute binary
  paths per target, install offers, substitution when an agent is
  missing, and multi-turn follow-ups in the same pane.
- Workspaces provisioned from templated recipes (empty, git clone,
  existing directory); reopen, archive, delete and repair.
- Local and SSH targets (through a Tailscale sidecar), a target health
  probe, and per-target policy: purpose (personal or work), allowed
  agents, provisioning, shell commands, confirmation before new work.
- Async dispatch jobs with a progress card, cancel, failed-turn retry,
  restart recovery that re-attaches to a running agent, and late agent
  output relayed after a turn ended.
- Approvals from the UI: an agent's own prompts (approve, deny, pick an
  option) and the router's offers (Approve / Deny cards).
- A freshly started agent is told the earlier conversation, limited to
  turns on targets of its own purpose.
- ntfy notifications (done, failed, needs you) linking to the
  conversation; the web client installs as a PWA.
- The web client reports its version and can update to the bundle
  loomux/server main pins, with rollback (off unless configured).
- Prometheus metrics, health probes, nightly SQLite backups and
  pre-migration snapshots.

### Security
- Vault secrets reach an agent through a 0600 file the launch sources and
  deletes, never on its command line.
- Agent output is redacted (vault values and secret-shaped tokens) before
  it goes to the relay model, and in stored transcripts.
- HTTP server timeouts against slow-header clients.
- The image build checks the web bundle against a sha256 pinned in this
  repository.

### Known limitations
- No dispatch audit trail (LOOM-110), no bracketed-paste delivery
  (LOOM-111), and no target onboarding API (LOOM-114).
- The relay model still sees non-secret agent output from every target,
  work machines included; a per-target setting is planned.

[Unreleased]: https://github.com/loomux/server/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/loomux/server/releases/tag/v0.4.0
[0.3.0]: https://github.com/loomux/server/releases/tag/v0.3.0
[0.2.0]: https://github.com/loomux/server/releases/tag/v0.2.0
[0.1.0]: https://github.com/loomux/server/releases/tag/v0.1.0
