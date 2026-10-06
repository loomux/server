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

[Unreleased]: https://github.com/loomux/server/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/loomux/server/releases/tag/v0.2.0
[0.1.0]: https://github.com/loomux/server/releases/tag/v0.1.0
