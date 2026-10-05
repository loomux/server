# Operating Loomux (LOOM-128)

Day-to-day procedures for a running deployment: upgrading, rolling
back, rotating secrets, and what to watch. Installation is
[`install.md`](install.md).

## Upgrading

Every merge to `main` is a patch release (`0.1.N`); see
[`../release/versioning.md`](../release/versioning.md). To upgrade:

1. Read the release notes between your version and the new one
   (<https://github.com/loomux/server/releases>). The notes don't flag
   schema changes; to see whether the upgrade migrates the database,
   compare `registry/sqlite/migrations/` between the two tags
   (`git diff --stat v0.1.A v0.1.B -- registry/sqlite/migrations/`).
2. Change the image tag (`ghcr.io/loomux/server:0.1.N`) and roll the
   Deployment. With `strategy: Recreate` the old pod stops first.
3. On start, `loomuxd` applies any new migrations. On the test
   instance an init container first copies the database to
   `loomux.db.pre-migration-<timestamp>` on **every** pod start (the
   last 5 are kept); that comes from its manifests, not from `loomuxd`,
   so a setup without one has no snapshot unless you take it. A turn
   that was running when the old pod stopped is picked up again if its
   agent session is still alive; if the session is gone the turn fails
   with "send your message again". A routing or provisioning step that
   was in flight is marked interrupted.
4. Check: `GET /api/v1/version` shows the new version,
   `GET /api/v1/health` says healthy, and the dashboard loads. The web client is part of the image, so it
   updates too; an open browser tab picks it up on reload.

The test instance's deploys go through a script that changes exactly
one line, the image tag, in its GitOps repository; any setup that pins
the tag in a manifest works the same way.

## Rolling back

- **No schema change between the two versions:** set the image tag back
  and roll. That's all.
- **The newer version migrated the database:** older code may not
  understand the new schema, and down-migrations aren't a supported
  path. Restore the database as it was before the upgrade:
  1. Suspend Flux (or whatever reconciles the Deployment) and set the
     image tag back first, so nothing restarts the new version.
  2. Pick the `loomux.db.pre-migration-<timestamp>` whose timestamp is
     **before the first start of the upgraded pod**, not simply the
     newest: a snapshot is taken on every start, so after one restart
     of the upgraded pod the newest one is already migrated, and after
     five the pre-upgrade one is gone. Copy it off the volume before
     anything else restarts.
  3. Stop `loomuxd`, restore that copy (the procedure is in
     [`backup-restore.md`](backup-restore.md#restore-a-backup)), start
     the older image, resume Flux. Anything done since the upgrade is
     lost.

## Rotating secrets

| Secret | How | Effect |
|---|---|---|
| Login password (`LOOMUX_AUTH_PASSWORD_HASH`) | `loomuxd -hash-password` for the new one, update the Secret, restart | Existing sessions stay valid until they expire (30 days of inactivity) or are revoked: revoke them under the API's `DELETE /api/v1/sessions/{id}` (list with `GET /api/v1/sessions`) |
| Router API keys | update the Secret, restart | none |
| ntfy token | update the Secret, restart | none |
| SSH key | update the SSH Secret (and the target's `authorized_keys`), restart | none |
| Vault master key (`LOOMUX_MASTER_KEY`) | **not rotatable in place**: credentials in the vault are encrypted with it. Today nothing writes to the vault (there's no API or UI for credentials yet; agents use their own logins on the targets), so while the vault is empty a new key loses nothing. Once any credential row exists, a different key makes reading the vault fail, and then **every** dispatch fails until those rows are deleted | — |

## What to watch

- **Health:** `GET /api/v1/health` (unauthenticated: database and router
  model; the probe to use) and `GET /api/v1/health/deep` (needs a
  session: every target, and the Tailscale sidecar).
- **Metrics** on `LOOMUX_METRICS_ADDR` (Prometheus): dispatches by
  outcome and stage timings (`loomux_dispatch_total`,
  `loomux_dispatch_stage_seconds`), router calls, tokens and escalations
  (`loomux_router_*`), tasks by status (`loomux_tasks`), target
  reachability (`loomux_target_up`) and operation times, and reaped
  tasks. The full list is in [`container.md`](container.md#metrics-loom-103).
- **Alerts** the test instance runs (Prometheus rules → Alertmanager →
  ntfy): dispatch failure rate high, no successful dispatch for a
  while, router error rate high, a target unreachable, a task stuck
  running, the pod restarting.
- **Logs:** one JSON record per line on stderr. Every turn logs
  `dispatch started` / `routing decision` / `dispatch finished` (or
  `dispatch failed` with `stage` and `error`) with `conversation_id`
  and `dispatch_id`; the failure's class is a metric label
  (`loomux_dispatch_total{error_class=…}`), not a log field; `LOOMUX_LOG_LEVEL=debug` adds more.
- **Disk:** the database grows with conversations and per-turn
  transcripts; transcripts older than `LOOMUX_TURN_RETENTION` (30 days
  by default) are deleted.

## Backups

Nightly online SQLite backup, volume snapshots, and pre-migration
copies: [`backup-restore.md`](backup-restore.md). Keep the vault master
key with the backups, and restore one now and then to know it works.

## Common problems

- **"target … can't be used right now".** The health probe failed:
  SSH, tmux, or disk space on that target. The Targets page shows
  which; `POST /api/v1/targets/{id}/probe` re-checks.
- **An agent asks for a login.** Its CLI on the target needs a manual
  `/login` (Claude Code) or `codex login` as the target user.
  Loomux never handles agent logins.
- **A turn stops at "needs you".** The agent is at a prompt: answer it
  from the card in the conversation, or attach with the command shown
  and answer it in the terminal.
