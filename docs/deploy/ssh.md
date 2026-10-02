# SSH access to remote targets

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
is starting without SSH material and continues — local targets still work.

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

## Follow-up: env-driven SSH options

Plumbing `WithIdentityFile` / `WithExtraSSHArgs` through
`targets.NewExecutor` so the identity path is env-driven is a reasonable
follow-up, but it does **not** remove the need for a real `$HOME/.ssh`: the
`ProxyCommand` and `known_hosts` have no equivalent as executor options,
and they are the two things most likely to be wrong. It would decouple the
key path from the mount layout, nothing more.

Once SSH works, prepare each target with the operator checklist in
`targets.md` (agent CLIs and login, one-time Claude Code workspace trust,
prompt visibility on shared hosts).
