# Docker plugin (LOOM-180, LOOM-178 PR 5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** the second target provider: `plugins/docker`, a container per machine on one Docker host reached
over SSH (`docker system dial-stdio`) or a local socket (design §9), passing the conformance suite and an
end-to-end run against the CI runner's own Docker, bundled in the server image and published as
`ghcr.io/loomux/plugin-docker`.

**Architecture:** its own Go module (`replace ../..`, pure Go: `x/crypto/ssh`, `x/net/proxy`, the Engine API
over `net/http`); `sdk`'s `Plugin` + `TargetProvider`. The named volume `lx-<id>-ssh` is the machine's record
(the spec without key material, the creation time and the fixed ssh port as labels; sshd's two files as its
contents) and the analogue of the Kubernetes Secret; the container of §9 field by field, mounting that volume
read-only; `lx-<id>-data` for a persistent machine's `/data`, an anonymous volume otherwise.

**Tech Stack:** Go 1.27, Engine API v1.41 (Docker 20.10+), `golang.org/x/crypto` v0.57.0, `golang.org/x/net`
v0.60.0, distroless static for the plugin image, the runner's Docker in CI.

**Spec:** `docs/design/target-providers.md` §9, §2.2, §2.3, §4, §12 "Docker plugin".

## What Docker does differently from §9 (found on Docker 29.8, 2026-10-10; each becomes a §9 edit)

1. **`PUT /containers/{id}/archive` is refused on a `--read-only` container** ("container rootfs is marked
   read-only"), and so is an upload into a read-only volume mount. The daemon does extract into a read-write
   volume of a *created, not started* container. So sshd's files go into the named volume `lx-<id>-ssh`,
   written through a transient helper container `lx-<id>-init` (created, never started, the volume mounted
   read-write at `/ssh`), and the agent container mounts that volume read-only at `/etc/loomux/ssh-src`.
2. **An ephemeral host port (`HostPort: "0"`) is reallocated at every start** (32768, then 32769 after a
   stop/start). The port must be fixed by the plugin: `create` probes one by starting the helper with
   `--publish <bind>:0:2222` (`sleep infinity`, the same hardened flags), reads it back, removes the helper,
   and bakes it into the record and the container's `HostConfig` (`HostPort: "<port>"`), so stop/start and
   recreate keep it. A start refused with "port is already allocated" / "address already in use" makes
   `create` rebuild the record with a fresh probe (three attempts).
3. **A port published on an `--internal` network isn't mapped at all** (`NetworkSettings.Ports` empty, "no
   public port"), so sshd in an egress-`none` container would be unreachable. `egress: none` is withdrawn:
   the manifest declares no `targets.egress_policy`, `targets.describe` offers `["internet"]` only, a spec
   asking for `none` is refused. Flagged in the handoff as a product decision.
4. `--init`: sshd is process 1 and reaps nothing, so orphans would pile up as zombies against
   `--pids-limit`; the container runs with `Init: true` (docker-init, part of every Docker install).
5. `--security-opt seccomp=default` isn't a value the API takes (the CLI reads a file); the daemon's default
   profile applies by itself, and `check` warns (`seccomp_disabled`) when `/info` lists no seccomp.
6. A HEALTHCHECK (`bash -c 'exec 3<>/dev/tcp/127.0.0.1/2222'`, every 5 s) is the readiness probe's
   analogue: `running` means healthy.
7. The image isn't pulled by the engine on its own: a missing image is pulled by the plugin in the
   background; `create` waits up to 20 s, then answers `creating` ("pulling the image") and finishes the
   creation when the pull ends; `get` reports the progress. The host's reconcile resumes a create a plugin
   restart dropped.

## Global Constraints

- The container of §9: `User 10002:10002`, `ReadonlyRootfs`, tmpfs `/tmp` (1g) and `/run/loomux` (1m),
  `CapDrop [ALL]`, `SecurityOpt [no-new-privileges:true]`, `PidsLimit 512`, memory and cpus from the size,
  `Init`, `RestartPolicy unless-stopped`, the `loomux-agents` network (`enable_icc=false`), nothing from the
  host mounted, no `Privileged`, no `Binds`, no host namespaces, no `LOOMUX_*` env (asserted by a test).
- The plugin never receives or logs loomuxd's keys; its own `ssh_private_key` never appears in an error.
- No pin, no connection: an unpinned docker host is scanned by `check` only (the key recorded, the handshake
  refused before authentication) and reported as `host_key_unpinned`; `targets.*` answer `unavailable`.
- Every `targets.*` method idempotent on its id; another instance's objects never listed or touched; every
  object labelled `loomux.io/instance`.
- The Engine API is reached through exactly these endpoints (the README lists them): `_ping`, `version`,
  `info`, `networks` (list/create/inspect), `volumes` (list/create/inspect/remove), `images/{ref}/json`,
  `images/create`, `containers` (create/inspect/list/start/stop/remove/archive PUT/logs).

## Review Focus

1. A stopped persistent machine's address: `get` answers the record's port and `stopped` (the data volume
   is there), so the target's pin never moves (`TestLifecycle/stopped keeps the port`).
2. A create interrupted after the probe but before the container: the next `create` finds the record,
   reuses its port, re-uploads and finishes (`TestCreateResumes`).
3. A port taken between the probe and the start: `create` rebuilds the record with a new port rather than
   failing forever (`TestCreateRetriesTakenPort`).
4. A lost ephemeral container: `get` says `lost`; a lost persistent container: `stopped`, and `start`
   remakes it from the record (`TestLostContainer`).
5. The host's key line in either form (`ssh-ed25519 AAAA…` or a known_hosts line): both pin; a mismatch is
   `host_key_mismatch`, never a connection (`TestHostKeyPin`).

---

### Task 1: Module, manifest, config, Engine client, transports
- [ ] `go.mod`/`go.sum`, `plugin.json`, `config.go` (engine URL, bind_address an IP that isn't unspecified,
  sizes with Kubernetes-style quantities, `ssh_proxy`, `proxy`), `quantity.go`.
- [ ] `engine.go`: the client over an `http.Transport` whose `DialContext` is the engine's dialer; errors
  mapped (404 not_found, 401/403 unauthorized, 409/5xx unavailable with the daemon's message, bounded).
- [ ] `transport.go`: `unixDialer`, `sshDialer` (x/crypto/ssh, the plugin's key, `FixedHostKey`, optional
  SOCKS5, one client reconnected on loss, a session per connection running `docker system dial-stdio`),
  host-key scan.
- [ ] Tests: config (defaults, rejections), quantities, the ssh transport against `targets/sshtest` with
  its new `Exec` hook piping to a fake engine on a unix socket, the scan and the mismatch.

### Task 2: Objects, status, targets.*, check
- [ ] `objects.go`: names, labels, record labels/spec, `containerFor` (§9 + the findings), `helperFor`,
  the tar of the two files (uid/gid 10002, 0400).
- [ ] `status.go`: inspect → status, digest via `RepoDigests`, the log tail for an error reason.
- [ ] `targets.go`: describe, create (the op machinery: probe, record, upload, data volume, container,
  start; background pull), get, list (records), start, stop, recreate, destroy, health, attach_commands.
- [ ] `plugin.go`: Describe/Configure/Check (`engine_unreachable`, `unauthorized`, `host_key_unpinned`,
  `host_key_mismatch`, `engine_too_old`, `seccomp_disabled`, `image_missing`, `network_misconfigured`).
- [ ] Tests against the in-memory fake engine (`fakeengine_test.go`): the container's security fields, the
  lifecycle, resume, the taken port, lost containers, another instance invisible, check cases, the
  conformance suite in-process. `cmd/loomux-plugin-docker`.

### Task 3: Host-side changes, images, workflow, docs
- [ ] `plugins/plugintest/targets.go`: an address template without `{id}` is a fixed host.
- [ ] `targets/sshtest`: `Exec` hook.
- [ ] `plugins/docker/Dockerfile` + `smoke.sh`; server `Dockerfile` stage 1b bundles the plugin.
- [ ] `docker_test.go` (`-tags docker`): the binary against the runner's Docker over the socket
  (conformance, end to end with a real ssh login on `127.0.0.1:<port>`, stop/start/recreate keeping the
  port, destroy leaving nothing), and through `sshtest` running the real `docker system dial-stdio`.
- [ ] `plugins.yml`: `plugin-docker` job (unit, govulncheck, integration against the runner's Docker, image,
  smoke, scan) and the publish matrix; paths.
- [ ] README (what it does with the socket, endpoint by endpoint), `docs/deploy/plugins.md`, design §9
  edits, `changes/loom-180-docker-plugin.md`.
- [ ] Local integration run green; PR; handoff for review; merge on approve + green CI; handoff.
