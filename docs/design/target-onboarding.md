# Target onboarding without an SSH config (LOOM-138)

**Status:** approved 2026-10-08 with all five recommendations below · **Ticket:** LOOM-138 (Vikunja Loomux #105 / id 1433)
**Builds on:** LOOM-114 (host-key scan/pin/test, migration `00021_target_ssh`), the credential vault's
AES-256-GCM encryption (`registry/sqlite/crypto.go`, `LOOMUX_MASTER_KEY`).

## What main has today (read at `c148af5`)

- `targets.NewExecutor` builds `ssh … -- user@host /bin/sh` with no identity, no proxy and no host name of its
  own. Everything else comes from `~/.ssh` in the container, which `deploy/entrypoint.sh` copies from the
  `22-loomuxd-ssh` SOPS secret: `config` (`IdentityFile`, the SOCKS5 `ProxyCommand nc -X 5 -x 127.0.0.1:1055
  %h %p` to the userspace Tailscale sidecar, possibly `HostName` aliases), the private key and `known_hosts`.
- LOOM-114 already added, *on top of* that config: `ssh_port`, host-key `scan-host-key` → `pin` (TOFU confirmed
  by fingerprint, 10-minute scan TTL) → `test`, and a per-target known_hosts file (`known_hosts.d/<id>`) checked
  with `StrictHostKeyChecking=yes`.
- `targets.ssh_key_ref` exists in the schema, is unused, and was removed from the API at the v1 freeze.
- So three things still only come from the secret: **the key**, **the proxy**, and **the real host name**
  behind an alias. That is what this ticket moves into Loomux.

## Model: two connection modes per target

| | `config` (today, unchanged) | `managed` (new) |
|---|---|---|
| Selected by | `ssh_key_ref = ''` | `ssh_key_ref` names a Loomux-managed key |
| ssh_config | mounted `~/.ssh/config` applies | **`-F /dev/null`**: no config file is read |
| Identity | from the config | the managed key, via an in-process agent (below) |
| Proxy | from the config | the server's `LOOMUX_SSH_PROXY`, unless the target opts out |
| Host keys | pin if any, else mounted known_hosts | **pin required**; no pin → refuse to connect, with a clear error |
| Host | may be an alias | must be the real DNS name / IP (strict grammar) |

Existing targets stay in `config` mode and keep working byte-for-byte; nothing changes for them until they are
migrated. New targets created in the UI are `managed`.

### Data model (migrations `00023_ssh_keys`, PR 1, and `00024_target_ssh_proxy`, PR 2)

```sql
CREATE TABLE ssh_keys (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  type        TEXT NOT NULL,           -- 'ssh-ed25519'
  public_key  TEXT NOT NULL,           -- authorized_keys line
  fingerprint TEXT NOT NULL,           -- SHA256:…
  private_key BLOB NOT NULL,           -- AES-256-GCM(nonce||ct), AAD = 'ssh_key:'||id
  origin      TEXT NOT NULL,           -- 'generated' | 'imported'
  created_at  TIMESTAMP NOT NULL
);
-- 00024:
ALTER TABLE targets ADD COLUMN ssh_proxy TEXT NOT NULL DEFAULT '';  -- '' = server default, 'none'
```

`targets.ssh_key_ref` (existing column) holds an `ssh_keys.id`; a foreign key isn't addable to the existing
column in SQLite, so triggers enforce it: a target can only name an existing key, and a key in use can't be
deleted (409). Nothing ever read `ssh_key_ref` before, so 00023 clears whatever an early client stored there;
from then on a non-empty value means `managed`. No new column for the host —
`host` simply must be real in managed mode.

### Key storage

- Generated in-process (`crypto/ed25519`), stored encrypted with the vault's master key (`LOOMUX_MASTER_KEY`),
  with the row id as GCM additional data so a ciphertext can't be swapped onto another key row.
- Without a master key, managed-mode endpoints answer `503 "SSH key storage needs LOOMUX_MASTER_KEY"`;
  `config`-mode targets are unaffected.
- **Never on disk in plaintext.** Each managed key is served by its own in-process SSH agent
  (`golang.org/x/crypto/ssh/agent`) on a unix socket in a 0700 directory under `$TMPDIR/loomux/agents/`;
  ssh gets `-o IdentityAgent=<socket> -o IdentitiesOnly=yes -i <public key file>`. The agent is read-only
  (list and sign; add/remove/lock refused). Keys are decrypted once at first use and dropped when the key is
  deleted.
- Default: **one key per target**, generated with the target and named after it (comment
  `loomux-<instance>-<target>`). A target may instead reuse an existing key (`ssh_key_id`), e.g. a whole fleet
  authorized once. Per-target keys mean revoking one machine's access never touches another.
- Rotation: generate a new key for the target → shown public key → user authorizes it → `test` with the new
  key → switch (`PUT ssh_key_id`) → user removes the old line. Master-key rotation is out of scope (same as the
  credential vault today).

### Building the ssh argv (managed)

Fixed option list, no value from the API inserted unvalidated, destination after `--`:

```
ssh -F /dev/null -o BatchMode=yes -o LogLevel=ERROR -o ConnectTimeout=… -o ServerAlive… -o ControlMaster/Persist/Path=…
    -o IdentityAgent=<sock> -o IdentitiesOnly=yes -i <pub> -o PreferredAuthentications=publickey
    -o UserKnownHostsFile=<known_hosts.d/id> -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes
    -o ProxyCommand=<loomuxd -ssh-proxy-connect <proxy> %h %p>   # only when a proxy applies
    -p <port> -- <user>@<host> /bin/sh
```

- **Proxy** is server configuration only (`LOOMUX_SSH_PROXY=socks5://127.0.0.1:1055`, parsed as a URL at
  startup, rejected unless `socks5://host:port`), never an API field; a target can only choose default or
  `none`. The `ProxyCommand` runs loomuxd's own binary as a SOCKS5 relay (a hidden flag using
  `golang.org/x/net/proxy`), so the command line is fixed and the image no longer needs `netcat-openbsd`.
  `%h`/`%p` are expanded by ssh from the strictly validated host/port.
- **Host grammar (managed mode):** an RFC 1123 host name (labels `[A-Za-z0-9-]`, no leading `-`, ≤253) or an
  IP literal parsed by `net.ParseIP`. No `%`, `;`, `$`, quotes, whitespace, `@`, `/`. `config`-mode targets keep
  today's (looser, already `--`-protected) validation so no stored target becomes invalid.
- `ScanHostKey` uses the same builder (proxy, port, `-F /dev/null`) for managed targets.

## API additions (v1, additive only)

New fields on target responses:

```jsonc
"ssh_mode": "managed" | "config",
"ssh_key": { "id": "…", "name": "…", "type": "ssh-ed25519", "fingerprint": "SHA256:…",
             "public_key": "ssh-ed25519 AAAA… loomux-test-wyzer" } | null,
"ssh_proxy": "default" | "none",
"ready": false, "next_step": "pin_host_key" | "authorize_key" | null   // onboarding progress
```

New optional request fields on `POST`/`PUT /targets`: `ssh_key_id` (attach an existing key),
`generate_ssh_key: true` (create one for this target), `ssh_proxy`. Changing `host` or `ssh_port` of a managed
target **drops its pin** (a new address must be confirmed again); response says so.

New endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/ssh-keys` | list keys: public parts and which targets use them |
| `POST /api/v1/ssh-keys` `{name}` | generate an ed25519 key; returns public parts only |
| `DELETE /api/v1/ssh-keys/{id}` | 409 while a target uses it |
| `POST /api/v1/targets/{id}/migrate-ssh` `{dry_run}` | config → managed, see Migration |

`POST /targets/{id}/test` gains (additively) `steps`: `connect` (proxy/TCP), `host_key`, `auth`, `tmux`, each
`ok`/`failed`/`skipped` with a plain-language `error` and `hint` — e.g. auth failed →
"Add the public key to ~user/.ssh/authorized_keys on host", host key unknown → "Scan and pin the host key".
Existing `reachable`/`error`/`host_key_problem` fields stay as they are.

The private key is never in any response type (the response structs have no field for it), any log line, or
any error text.

## Web flow (loomux-web, separate PR)

Targets page → **Add target**: (1) name, host, port, user, purpose → creates it with a generated key;
(2) **Verify host**: scan, show type + fingerprint next to the command to compare it
(`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), explicit **Trust this key** button; (3) **Authorize
Loomux**: the public key with a copy button and the one-liner to append it to `authorized_keys` (with
`no-port-forwarding,no-agent-forwarding,no-X11-forwarding` prefixed); (4) **Test connection** with the step list.
Edit (host/port/user/proxy, rotate key), remove (confirm; offers to delete the now-unused key). `config`-mode
targets show a "Uses the deployment's SSH config — Migrate" badge.

## Migration of existing targets

`POST /targets/{id}/migrate-ssh` runs inside the server, where the mounted config is:

1. `ssh -G <host>` (with the target's port/user) resolves the effective `hostname`, `port`, `user`,
   `identityfile` and whether a `proxycommand` applies — this resolves aliases without anyone reading the
   secret by hand.
2. Host key: the existing pin if any, else the mounted `known_hosts` entry for that host (`ssh-keygen -F`).
3. Key: imports the mounted private key file `ssh -G` named into `ssh_keys` (`origin: imported`, one row shared
   by every migrated target that uses the same file, matched by fingerprint).
4. `dry_run: true` returns the plan (resolved host/port/user, host-key fingerprint, key fingerprint, proxy
   default/none, and anything that doesn't map — e.g. a non-SOCKS `ProxyCommand`, `ProxyJump`, extra options),
   changing nothing. Applying it writes the target, then runs `test` in managed mode; on failure it rolls the
   target back to `config` mode and reports why.

Migrated targets need no change on the machines themselves (same key, same host key). Fresh per-target keys
can then be rotated in at leisure. Once every target is managed and tested, **retiring the seeded secret** is
a separate step that needs the user's OK: a handoff to command-center to drop `22-loomuxd-ssh` (the Tailscale
secret stays) and set `LOOMUX_SSH_PROXY`; the entrypoint's copy step stays harmless when the mount is absent.

## Threat model

| Asset / threat | Mitigation |
|---|---|
| Private keys = shell on every target | encrypted at rest with the master key (held outside the DB); plaintext only in loomuxd memory; never in responses, logs, errors, or files; DB backups alone don't expose them |
| SSH option/argument injection (cf. #246) | `-F /dev/null`; fixed option list; host/user/port strictly validated; `--` before the destination; proxy command fixed and from server env only; no free-form ssh option anywhere in the API |
| `%`-token / shell expansion in `ProxyCommand` | `%h` only ever expands a validated host (no `%`, shell metacharacters); OpenSSH ≥ 9.6 also refuses such names |
| Host substitution (MITM, DNS, a hijacked tailnet name) | managed mode refuses to connect without a pin; pin only from a fresh scan by fingerprint, explicitly confirmed; host/port change drops the pin |
| Ciphertext swapping between rows | GCM AAD binds each ciphertext to its key id |
| Agent socket abuse | 0700 directory, read-only agent, one key per socket; not exported to any child environment. Residual: processes running as the loomuxd uid **inside the container** (agents on the *local* target) can reach the socket — they can already read the DB and the master key env, so this adds nothing; documented |
| Who may pin / add keys | every endpoint is behind `requireAuth`; Loomux has one user. With several users these become admin-only (as LOOM-114 already notes) |
| Lost master key | managed targets fail with a clear error; keys must be regenerated and re-authorized; documented in backup-restore |

Negative tests planned: hosts `-oProxyCommand=x`, `a;id`, `$(id)`, `` `id` ``, `%d`, `h\nx`, IPv6 forms; user
and key-name injection; `ssh_proxy` values other than `default`/`none`; a JSON-response scan for `PRIVATE KEY`
and the base64 of the seed on every endpoint; log capture asserting no key material; AAD swap test; managed
target without a pin refused before ssh runs; a fake `ssh` on `PATH` asserting the exact argv.

## Phasing

1. **Server PR 1** — `ssh_keys` store + encryption + agent + `/ssh-keys` endpoints.
2. **Server PR 2** — managed mode in the executor and scan, the SOCKS5 relay, `LOOMUX_SSH_PROXY`, target fields,
   strict host grammar, test steps.
3. **Server PR 3** — `migrate-ssh` (dry run + apply), docs (`ssh.md`, `targets.md`, `backup-restore.md`).
4. **Web PR** — onboarding wizard, edit/remove/rotate, migrate badge.
5. Test-instance deploy, migrate its targets, verify; then (with the user's OK) the secret-retirement handoff.

## Decisions for the user

1. **Keys:** one generated key per target by default, reuse optional *(recommended)* — or one instance-wide key.
2. **Migration:** import the seeded key so machines need no change, rotate later *(recommended)* — or generate
   fresh keys and authorize each machine by hand during migration.
3. **Proxy:** server-wide `LOOMUX_SSH_PROXY` + per-target `none`, implemented by loomuxd as its own SOCKS5 relay
   *(recommended)* — or keep `nc -X 5` as the `ProxyCommand`.
4. **Key handling at runtime:** in-process agent, never on disk *(recommended)* — or 0600 temp files like the
   pinned known_hosts.
5. **Managed targets require a pin** before any connection, and a host/port change drops it *(recommended)*.
