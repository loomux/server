# The Docker plugin (LOOM-180)

A target-provider plugin (`docs/design/target-providers.md` §9): one
container per machine on one Docker host, reached by loomuxd over SSH
at a port published on the host's address. The plugin talks to the
engine over SSH as well (`docker system dial-stdio` on the host, what
the Docker CLI's `ssh://` contexts do) or through its local socket. Its
own Go module, so `x/crypto/ssh` and `x/net/proxy` never enter the
server's `go.mod`; it replaces `github.com/Loomux/server` with `../..`.

```sh
cd plugins/docker
go test ./...                       # unit tests and the conformance suite, against a fake engine
go build ./cmd/loomux-plugin-docker
LOOMUX_DOCKER_ENGINE=unix:///var/run/docker.sock LOOMUX_DOCKER_AGENT_IMAGE=loomux-agent:dev \
  go test -tags docker -count=1 -timeout 15m -run TestDocker -v ./...   # against a real engine
```

## Forms

- **Bundled** in the server image under `/usr/local/lib/loomux/plugins/docker/`
  (`Dockerfile` stage 1b): installed from the UI as source `bundled`,
  run as a subprocess.
- **Sidecar**: `ghcr.io/loomux/plugin-docker` (`plugins/docker/Dockerfile`,
  distroless), started with `--listen /run/loomux/plugins/docker.sock`
  beside loomuxd. Its only credential is the SSH key in its
  configuration; nothing is mounted.

## Configuration

`plugin.json`'s schema: `engine` (`ssh://user@host[:port]`, the user in
the docker group, or `unix:///var/run/docker.sock`; nothing else: a
`tcp://` engine without TLS client certificates is an open root shell),
`ssh_private_key` (secret; the plugin's own key, never loomuxd's
managed keys), `ssh_host_key` (the docker host's key line; leave it
empty, run the check, and trust the key it scanned: until then the
plugin never connects), `proxy` (`socks5://host:port` for a docker host
only on the tailnet), `bind_address` (one IP of the docker host, never
`0.0.0.0`: what machines publish sshd on and what loomuxd connects
to), `ssh_proxy` (`default` or `none`: how loomuxd reaches the
machines), `agent_image`, `sizes` (`{name: {cpu, memory, disk}}` in
Kubernetes quantities; cpu and memory become `--cpus` and `--memory`,
disk is advisory since a volume isn't capped), `max_environments`,
`timezone`.

## What it does with the engine

The engine is **root-equivalent on the docker host** (design §4): the
docker group is root there, and so is a socket. The plugin uses
exactly these endpoints of API v1.41 and nothing else, and every object
it makes carries its labels (`loomux.io/managed-by=loomux-plugin-docker`,
`loomux.io/instance`, `loomux.io/environment`, `loomux.io/role`), so it
only ever touches its own:

| Endpoint | For |
|---|---|
| `GET /_ping`, `GET /info` | the check: the engine answers (`_ping`'s `API-Version` is 1.41+), Linux containers, seccomp on |
| `GET /networks/loomux-agents`, `POST /networks/create` | the one user-defined bridge, `enable_icc=false`, made once per host |
| `GET /volumes`, `GET /volumes/{name}`, `POST /volumes/create`, `DELETE /volumes/{name}` | the record volume `lx-<id>-ssh` and the data volume `lx-<id>-data`, by label |
| `GET /images/{ref}/json`, `POST /images/create` | is the agent image there; pull it when not |
| `POST /containers/create`, `GET /containers/{name}/json`, `GET /containers/json?all=1&filters=…` | the machine's container and its transient helper, by name, checked for its labels; the listing finds a container of ours whose record is gone, reported `lost` so the host's orphan sweep destroys it |
| `POST /containers/{name}/start`, `/stop`, `DELETE /containers/{name}?force=1&v=1` | lifecycle |
| `PUT /containers/{name}/archive` | sshd's two files into the record volume, through the helper |
| `GET /containers/{name}/logs?tail=5` | the last line a failing container wrote, for its error reason |

What it never does: no `--privileged`, no capability kept, no bind
mount of anything from the host (the socket included), no host
namespaces, no `exec` into a machine, no image build, no network other
than its own. A unit test asserts the container's configuration field
by field, including the JSON the engine receives.

## What it makes per machine

| Object | Name | |
|---|---|---|
| Volume | `lx-<id>-ssh` | **the record**: labels carry the spec (without key material), the creation time and the fixed ssh port; contents are `host_ed25519` and `authorized_keys` (0400, the agent's), mounted read-only at `/etc/loomux/ssh-src`. The machine's host private key therefore sits on the docker host's disk (`/var/lib/docker/volumes`, readable by root and the docker group) for the machine's life, as the Secret sits in etcd on Kubernetes. While the record exists the machine exists: `stopped` when its container is gone and its data volume stays, `lost` otherwise |
| Volume | `lx-<id>-data` | persistent machines only, `/data`; an ephemeral machine's `/data` is an anonymous volume removed with its container |
| Container | `lx-<id>` | the container of design §9: `--user 10002:10002`, `--read-only`, tmpfs `/tmp` (1g) and `/run/loomux` (1m), `--cap-drop ALL`, `no-new-privileges`, `--pids-limit 512`, `--memory`/`--cpus` from the size, `--init`, `--restart unless-stopped`, on `loomux-agents`, sshd published at `bind_address:<port>`, a health check (`bash -c 'exec 3<>/dev/tcp/127.0.0.1/2222'` every 5 s) standing in for the readiness probe: `running` means healthy |
| Container | `lx-<id>-init` | a transient helper, hardened like the machine, that sleeps: one probes the port, one (never started) receives the archive into the record volume; removed in the same call |

## What Docker made different from design §9 (found on Docker 29.8)

- `PUT /containers/{id}/archive` is refused on a `--read-only`
  container ("container rootfs is marked read-only"), so the files go
  into the record volume through the helper, and the machine mounts it
  read-only.
- A `HostPort` of `0` is allocated anew at every start, so the plugin
  probes a port once (the helper, `--publish bind:0:2222`), then fixes
  it in the record and the container; stop/start and recreate keep it.
  A probe that lands on a port a stopped machine of this instance has
  fixed (nothing holds it on the host, so the allocator can hand it out
  again) is repeated; a port found taken at start (another process in
  the gap) rebuilds the record with a fresh probe, three attempts.
- A port published on an `--internal` network isn't mapped at all, so
  an egress-`none` machine's sshd would be unreachable: **`egress: none`
  is withdrawn**. The manifest declares no `targets.egress_policy`,
  `targets.describe` offers `internet` only, and a spec asking for
  `none` is refused. Docker gives no network isolation anyway: a
  machine can reach the LAN and the tailnet its host is on; the docker
  host's own security is the user's.
- `--init`: sshd is process 1 and reaps nothing; orphans would pile up
  as zombies against `--pids-limit`.
- `--security-opt seccomp=default` isn't a value the API takes; the
  daemon's default profile applies, and the check warns
  (`seccomp_disabled`) when the engine runs without one.
- The image isn't pulled by the engine: a missing one is pulled by the
  plugin in the background. `create`, `recreate` (onto a new image, the
  usual case) and `start` remaking a lost container all run in the
  background the same way: the call answers `creating` / `recreating` /
  `starting` with the phase ("pulling the image") after 20 s (less when
  the host's call deadline is nearer) and finishes when the pull ends;
  `get` reports the progress. A destroy or a recreate cancels a making
  in progress and waits for it, so nothing is made behind its back; a
  plugin restart drops it, and the host's reconcile resumes what it
  needs.

## The SSH connection

One multiplexed connection per instance, a session per HTTP
connection. A connection that dies without a close (a tailnet path
change, a NAT timeout) is found out two ways: a keepalive every 15 s
that drops the client when unanswered for 10 s, and sessions opened
under the call's context, so a call on a dead connection fails at its
deadline, drops the client, and the next call dials anew. The
handshake asks for the pinned key's type (RSA with its SHA-2
signatures), so an ed25519 line pins a host that also has an RSA key.
The agents' network must be as the plugin makes it: an existing
`loomux-agents` without `enable_icc=false` is refused at create, not
used.

## The check

`engine_unreachable`, `unauthorized` (the host refused the plugin's
key: add its public half to the docker user's `authorized_keys`),
`host_key_unpinned` (the scanned key's type, fingerprint and the line
to put in `ssh_host_key`; the handshake is refused before any
authentication, and nothing else works until the key is pinned),
`host_key_mismatch`, `engine_too_old` (API < 1.41, Docker 20.10),
`engine_not_linux`; warnings `seccomp_disabled`, `image_missing` (the
first machine pulls it), `network_misconfigured` (`loomux-agents`
exists without `enable_icc=false`).

## Tests

`plugin_test.go`, `config_test.go`, `objects_test.go`, `targets_test.go`,
`check_test.go`: the manifest and schema; configuration (the engine URL,
the key, both host-key line forms, the proxy, sizes and quantities,
twenty-odd rejections, no error repeating the key); the container and
helper configurations field by field (and the JSON the engine gets);
the record's labels and archive; the lifecycle against an in-memory
engine (`fakeengine_test.go`, with Docker's port reallocation, archive
refusal, anonymous volumes and label filters): create idempotent, the
probed port, stop keeping the port, start, recreate keeping address and
data, destroy twice, resume after a crash, a taken port, a lost
container (persistent and ephemeral), the image pull (fast, slow,
failing), the quota, another instance invisible, status mapping, the
log tail; every check outcome, over the socket and over SSH
(`targets/sshtest` with its `Exec` hook relaying `docker system
dial-stdio`); and the host's conformance suite in-process.
`transport_test.go`: the dial-stdio session, reconnection, a missing
docker on the host, the host-key scan, mismatch and refusal, SOCKS5.
`docker_test.go` (`-tags docker`, CI's `plugin-docker` job): the binary
against a real engine over its socket and over SSH to the same machine:
the conformance suite, then create → `running` → an ssh login at the
published port with the host key pinned (`tmux -V`, uid 10002,
`/data/work` writable, docker-init as process 1) → stop → start at the
same port → recreate at the same port → destroy, nothing left.
