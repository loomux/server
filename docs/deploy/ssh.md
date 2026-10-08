# SSH access to remote targets

> **Two ways to reach a target (LOOM-138).** A target with a
> Loomux-managed SSH key (`ssh_mode: managed`) needs nothing below the
> next section: no `~/.ssh` at all. See [Managed targets](#managed-targets-loom-138).
> Everything else here is about **config-mode** targets, reached through
> the mounted SSH config, which is how every target registered before
> LOOM-138 still works.

`loomuxd` reaches a remote execution target by exec'ing the `ssh(1)` binary
(`targets/remote.go`), not by using an in-process SSH library. Production
constructs the remote executor with **no options**:

```go
// targets/targets.go
return NewRemoteExecutor(t.Host, t.User)
```

The `WithIdentityFile` / `WithPort` / `WithExtraSSHArgs` options exist, but
their own doc comment says why they are test-only:

> Production's `NewExecutor` passes none of these — they exist for tests to
> point at a non-standard host/port/identity … real targets are expected to
> resolve via the user's own `~/.ssh/config`, matching the design spec's
> `"ssh wyzer"`-style convention.

So **`$HOME/.ssh` is the configuration surface.** There is no environment
variable for the key, the port, or the proxy. Everything below is about
getting the right files into `$HOME/.ssh` inside the container, with the
permissions `ssh` insists on.

## What `$HOME/.ssh` must contain

`$HOME` is `/home/loomux`, so the directory is `/home/loomux/.ssh`, mode
`0700`, owned by uid 10001. Three files:

| File | Required | Why |
|---|---|---|
| `id_ed25519` (or any name you reference) | yes | the private key. Mode `0600`. |
| `config` | yes, in a Tailscale-sidecar deployment | carries `IdentityFile` and the SOCKS5 `ProxyCommand` |
| `known_hosts` | **yes** | production runs with `BatchMode=yes`; an unknown host key is a refusal, not a prompt |

### `config`

```
Host *
    IdentityFile ~/.ssh/id_ed25519
    IdentitiesOnly yes
    ProxyCommand nc -X 5 -x 127.0.0.1:1055 %h %p
```

`nc -X 5 -x HOST:PORT` speaks SOCKS5. `127.0.0.1:1055` is the Tailscale
sidecar running in **userspace mode**, which exposes a SOCKS5 proxy rather
than creating a `tun` device — so the pod has no route to the tailnet and
every SSH connection must be proxied.

This requires **`netcat-openbsd`**. BusyBox's `nc` has no `-X` flag and will
fail with a usage error that surfaces only as a failed dispatch. The image
installs the real one; `socat` is also present if you prefer:

```
    ProxyCommand socat - SOCKS5-CONNECT:127.0.0.1:1055:%h:%p
```

Narrow `Host *` to the specific target hostnames if the pod ever needs to
reach something that is *not* behind the proxy.

### `known_hosts`

Pre-seed it. `targets/remote.go` passes `-o BatchMode=yes` and does not
override `StrictHostKeyChecking`, so an unrecognised host key produces
`Host key verification failed` — no prompt, no TOFU. From a machine that
can already reach the target:

```sh
ssh-keyscan -H wyzer.example.internal > known_hosts
```

Verify the fingerprints against the host before trusting the output;
`ssh-keyscan` performs no authentication of what it collects.

This is the failure mode most likely to bite after a deploy that looked
healthy: the pod starts, serves the API and the UI, passes its probes, and
then fails the first remote dispatch. The entrypoint prints a warning at
startup when `known_hosts` is absent.

## How the files get there: a Secret, copied at startup

**Do not mount the Secret at `/home/loomux/.ssh`.** It will not work:

- A Secret volume is mounted **read-only**, **root-owned**, with
  `defaultMode` 0644 (and `ssh` refuses a private key that is
  group/world-readable or not owned by the caller — `UNPROTECTED PRIVATE
  KEY FILE`).
- `ssh` also wants to *write* to that directory in some configurations, and
  a read-only mount forecloses it.
- Mounting over `~/.ssh` would in any case hide anything the image put there.

Instead, mount it read-only somewhere else and let the entrypoint copy:

```yaml
volumeMounts:
  - name: loomux-ssh
    mountPath: /etc/loomux/ssh
    readOnly: true
volumes:
  - name: loomux-ssh
    secret:
      secretName: loomux-ssh
```

`deploy/entrypoint.sh` runs before `loomuxd` and copies every regular file
from `/etc/loomux/ssh` into `$HOME/.ssh`, `chmod 0600`. Copying is what
fixes the ownership problem: `cp` creates the destination owned by the
*running* uid, so no `chown` and no root are needed. It uses `cp -L`
because a Secret volume's entries are symlinks into `..data/`.

Override the source path with `LOOMUX_SSH_SOURCE` if `/etc/loomux/ssh` is
inconvenient. If the directory does not exist, the entrypoint logs that it
is starting without SSH material and continues: only local targets can
then work, and the image turns those off by default
(`LOOMUX_LOCAL_TARGETS`, see [targets.md](targets.md#local-targets)).

Building the Secret:

```sh
kubectl create secret generic loomux-ssh \
    --from-file=id_ed25519=./id_ed25519 \
    --from-file=config=./config \
    --from-file=known_hosts=./known_hosts
```

Key names become filenames, so they must match what `config` references.

## Pod requirements

**`runAsUser` / `runAsGroup` / `fsGroup` must be 10001.** Not a
preference — `ssh` calls `getpwuid()` and exits if the uid has no `/etc/passwd`
entry:

```
No user exists for uid 1000
```

The pod starts fine with a mismatched uid and fails on the first remote
dispatch. See `docs/deploy/container.md` for why the image cannot accept an
arbitrary uid the way a pure-Go image could.

**`/tmp` must be writable.** `sshExec` multiplexes connections with
`ControlMaster=auto` and puts the control sockets under
`${TMPDIR:-/tmp}/loomux/ssh-cm/`, creating that directory at 0700 on
demand. With `readOnlyRootFilesystem: true`, mount an `emptyDir` at `/tmp`.
A Unix socket cannot live on a `tmpfs`-less read-only path, and the failure
is a connection error, not a clear permissions message.

**`$HOME/.ssh` must be writable too.** The entrypoint *copies* the Secret
into `/home/loomux/.ssh` rather than using the mount directly, because a
projected Secret is mode 0644 and root-owned and `ssh` rejects it. That copy
needs a writable `$HOME`, so `readOnlyRootFilesystem: true` needs a second
`emptyDir`:

```yaml
securityContext:
  readOnlyRootFilesystem: true
  runAsUser: 10001
  runAsGroup: 10001
  fsGroup: 10001
volumeMounts:
  - { name: tmp,     mountPath: /tmp }
  - { name: ssh-run, mountPath: /home/loomux/.ssh }
volumes:
  - { name: tmp,     emptyDir: {} }
  - { name: ssh-run, emptyDir: {} }
```

`fsGroup` is what makes the `emptyDir` writable by uid 10001. Without this
mount the pod does not start at all — the entrypoint fails on its first
`mkdir` and never execs `loomuxd`:

```
mkdir: can't create directory '/home/loomux/.ssh': Read-only file system
```

That one is at least loud. It is listed here because the `/tmp` note above
otherwise reads as the *only* thing read-only root filesystems need.

## Verifying it, inside the pod

```sh
kubectl exec -it deploy/loomux -- sh -c '
  ls -la ~/.ssh
  nc -h 2>&1 | grep -- "-X proto"
  ssh -o BatchMode=yes -o ConnectTimeout=5 user@target true; echo "exit: $?"
'
```

- `~/.ssh` should be `drwx------` and each file `-rw-------`, owned by
  `loomux`.
- Exit **255** means unreachable — the proxy, the host key, or the key
  itself. Read the stderr: `Host key verification failed` is `known_hosts`;
  `Permission denied (publickey)` is the key or the remote
  `authorized_keys`; a `ProxyCommand` usage error is the wrong `nc`.
- Exit **0** means the whole path works, including the SOCKS5 hop.

To check the proxy in isolation, without involving the target's
authentication:

```sh
nc -X 5 -x 127.0.0.1:1055 -z target.example.internal 22; echo "exit: $?"
```

## Onboarding a target through the API (LOOM-114)

The mounted secret stays the default for every target. On top of it, a
target can be onboarded without touching the secret:

1. Register it (`POST /api/v1/targets`, `kind: remote`, `host`, `user`,
   and `ssh_port` if it isn't the SSH config's).
2. `POST /api/v1/targets/{id}/scan-host-key` connects the way Loomux
   does (the secret's `config`, its proxy, the target's port) and returns
   the host key's type and SHA256 fingerprint. It authenticates nothing
   and trusts nothing. Compare the fingerprint with the machine's own,
   run there: `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`.
3. `POST /api/v1/targets/{id}/pin {"fingerprint": "SHA256:…"}` pins that
   key, if it is one the latest scan (up to 10 minutes old) returned.
   From then on the target is checked against its pin **alone**
   (`StrictHostKeyChecking=yes`), ahead of the secret's `known_hosts`; the
   pin is kept in the database and written to
   `known_hosts.d/<target id>` beside it, on the data volume.
4. `POST /api/v1/targets/{id}/test` checks it: reachable over SSH, and
   tmux runs there.

When a host key changes (the machine was reinstalled), dispatches fail
with `host_key_changed`, whose hint says to scan and pin again; `test`
flags it as `host_key_problem`. `DELETE /api/v1/targets/{id}/pin`
returns the target to the secret's `known_hosts`.

The key Loomux authenticates with, and the proxy, still come from the
secret. Loomux has one user today; if it ever has several, scanning and
pinning must become admin-only, since whoever pins decides which machine
every later command reaches.

## Managed targets (LOOM-138)

A managed target is reached with a key Loomux generated and keeps,
and with **no ssh_config at all**: ssh runs with `-F /dev/null`, and
everything it needs is on its command line, built from the target's
validated fields and the server's own settings. Design and threat model:
`docs/design/target-onboarding.md`.

Onboarding one, in the web UI or through the API:

1. `POST /api/v1/targets` with `kind: remote`, `host` (the real host
   name or IP address: no alias resolves it; letters, digits and `-`,
   dot-separated), `user`, `ssh_port` if not 22, and
   `"generate_ssh_key": true` (or `ssh_key_id` of an existing key).
   The response has `ssh_key.public_key` and `next_step: pin_host_key`.
2. Scan and pin the host key, as above. A managed target **refuses to
   connect without a pin**; there is no `known_hosts` to fall back to.
   Changing its `host` or `ssh_port` drops the pin, so the new machine's
   key is confirmed again.
3. Add `ssh_key.public_key` to `~/.ssh/authorized_keys` of `user` on the
   target. Prefixing it with
   `no-port-forwarding,no-agent-forwarding,no-X11-forwarding ` is fine:
   Loomux needs none of them.
4. `POST /api/v1/targets/{id}/test`. Its `steps` say which of `connect`,
   `host_key`, `auth` and `tmux` failed, with the reason; the target's
   `next_step` turns null and `ready` true once a probe newer than the
   last change reaches it.

What reaches the target:

- **The key** never touches the disk in the clear. It is stored
  encrypted with `LOOMUX_MASTER_KEY` (so managed targets need it), and
  decrypted into an in-process ssh-agent that serves only that key, on a
  socket in a 0700 directory under `$TMPDIR/loomux/agents`. ssh is told
  `IdentityAgent=<socket>`, `IdentitiesOnly=yes` and the key's public
  half, so the mounted key, if any, is never offered. Password and
  keyboard-interactive authentication, and agent and port forwarding,
  are off.
- **The proxy** is `LOOMUX_SSH_PROXY` (`socks5://127.0.0.1:1055` for the
  userspace Tailscale sidecar). The `ProxyCommand` is loomuxd itself,
  relaying through it (`loomuxd -ssh-proxy-connect PROXY HOST PORT`), so
  `nc` isn't needed. A target with `"ssh_proxy": "none"` is reached
  directly. The proxy is server configuration only: the API can choose
  it or not, never set it.
- **The ControlMaster** of a managed target is its own: its path carries
  a tag of the key, proxy and pin, so a connection made through the SSH
  config, with another key, or under an earlier pin is never reused.

Deleting a key (`DELETE /api/v1/ssh-keys/{id}`, refused while a target
uses it) stops its agent. A lost `LOOMUX_MASTER_KEY` makes managed keys
unreadable: generate new ones and authorize them again.

## Follow-up: env-driven SSH options

Plumbing `WithIdentityFile` / `WithExtraSSHArgs` through
`targets.NewExecutor` so the identity path is env-driven is a reasonable
follow-up, but it does **not** remove the need for a real `$HOME/.ssh`: the
`ProxyCommand` has no equivalent as an executor option (per-target
`known_hosts` pins and ports now do, above),
and they are the two things most likely to be wrong. It would decouple the
key path from the mount layout, nothing more.

Once SSH works, prepare each target with the operator checklist in
`targets.md` (agent CLIs and login, one-time Claude Code workspace trust,
prompt visibility on shared hosts).
