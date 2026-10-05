# Changelog

All notable changes to Loomux (loomux/server, and the loomux/web client it
pins) are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Loomux uses
[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html): see
`docs/release/versioning.md`. Every merge to main is a patch release
whose notes are on its GitHub release; this file has the curated
milestones.

## [Unreleased]

### Changed

- The image's web client is web 0.1.3 (`web-fc1c9ab`): the chat scrolls
  to the newest message, clearer cancelled-turn and update wording
  (LOOM-130), bundles carry build-provenance attestations (LOOM-118).

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

[Unreleased]: https://github.com/loomux/server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/loomux/server/releases/tag/v0.1.0
