# Container image (LOOM-50)

`Dockerfile` at the repo root builds the `loomuxd` image published as
**`ghcr.io/loomux/server`**. That name is fixed: theWyseKube's manifests pin
it. Don't rename the image without coordinating that change.

```sh
# Build with the placeholder web page (no credentials, no extra network):
docker build -t ghcr.io/loomux/server .

# Build with a real web bundle already present in the build context:
docker build --build-arg WEB_DIST=.web-dist -t ghcr.io/loomux/server .
```

## Why the runtime base is not `scratch` or distroless

This is the single most important thing about this image, and it is not
obvious from the Go source.

`loomuxd` does not talk SSH in-process. It shells out to real binaries:

| Binary | Called from |
| --- | --- |
| `ssh` | `targets/remote.go` — every remote tmux operation, `FileExists`, `RemoveFile`, `RunOnce`, `ControlMasterAlive`, `Close` |
| `tmux` | `targets/local.go` — every local session operation |
| `sh` | `targets/local.go` — `RunOnce` |

`golang.org/x/crypto/ssh` is used only by the test harness
(`targets/sshtest`), never by production code paths.

The consequence: the idiomatic Go multi-stage Dockerfile — static binary
into `FROM scratch`, or distroless — **builds clean, passes CI, and then
fails on every single dispatch at runtime**, because `exec.CommandContext`
can't find `ssh`/`tmux`/`sh`. `go build` succeeding proves nothing about
this class of failure. The runtime stage therefore carries a real userland
(Alpine), and CI must verify the binaries actually resolve *inside* the
image, not just that the build succeeded.

`CGO_ENABLED=0` is still safe: the only plausible cgo dependency would be
SQLite, and this repo uses `modernc.org/sqlite` (pure Go). That also makes
the musl-vs-glibc question moot for the runtime base.

### Installed packages and why each is there

| Package | Reason |
| --- | --- |
| `openssh-client` | `ssh`, per above |
| `tmux` | `tmux`, per above |
| `busybox` (implicit) | `sh`, per above |
| `netcat-openbsd` | SOCKS5 `ProxyCommand` — see below |
| `socat` | Second SOCKS5 implementation, kept as a fallback |
| `ca-certificates` | Outbound HTTPS to the router model's API |
| `tzdata` | Timestamps in logs and the message store |

`netcat-openbsd` is **not** incidental. Reaching targets through a
userspace-mode Tailscale sidecar needs a SOCKS5 `ProxyCommand`, and the
`nc -X 5 -x host:port` form that provides it exists only in OpenBSD netcat.
BusyBox's built-in `nc` has no `-X` flag at all, so dropping this package
silently breaks every remote target the moment the sidecar is in the path.

## Sourcing the web bundle — the decision

The web client lives in a **separate repository** (`loomux/web`), and
`loomuxd` serves its build output itself (LOOM-33: any non-`/api/*` path,
with SPA fallback to `index.html`). So the image has to get a `dist/`
directory from somewhere. This was a real architectural choice, so the
rejected options are recorded here rather than just the winner.

**Chosen: a build-context path behind a build arg.**

```dockerfile
ARG WEB_DIST=deploy/web-placeholder
COPY --chown=loomux:loomux ${WEB_DIST}/ /srv/loomux/web/
```

Whoever builds the image is responsible for putting a built bundle in the
build context and pointing `WEB_DIST` at it. CI does this by downloading
the `loomux/web` release that `deploy/web-ref` pins (LOOM-58, below) into
`.web-dist/`. The default is
a committed placeholder page, so a plain `docker build .` works for anyone,
with no credentials and no network access beyond the Go module proxy and
the Alpine mirrors.

### Rejected: clone `loomux/web` inside a Dockerfile build stage

1. **It needed a cross-repo credential inside the build.** When this was
   decided `loomux/web` was private, and a workflow in `loomux/server` gets
   a `GITHUB_TOKEN` scoped to `loomux/server` only, so cloning it needed a
   separate PAT threaded into the build (`loomux/web` has been public since
   2026-10-05, but the other reasons stand). The obvious way to do that,
   `ARG GITHUB_TOKEN`, **bakes the token into the image history**, where
   anyone who can pull the image can read it back. Doing it safely requires
   BuildKit `--mount=type=secret`, which adds a hard builder requirement
   (the legacy builder, still the default in some environments, silently
   does not support it).
2. **It destroys reproducibility.** Cloning `main` means the same source
   commit of this repo produces a different image tomorrow. For an image
   theWyseKube pins by SHA, that defeats the point of pinning.

### Where CI gets the bundle: a `loomux/web` release (LOOM-58)

Until LOOM-58, CI checked out `loomux/web` at the pinned ref and built it
itself, because `loomux/web` published nothing. Now its CI publishes
every tested `main` commit as a GitHub pre-release `web-<short sha>`,
holding `loomux-web-<short>.tar.gz` (the built `dist/`) and
`web-release.json` (`commit`, `tarball`, `sha256`, …; format in
`loomux/web` `docs/release.md`).

`deploy/web-ref` names a `loomux/web` commit and `deploy/web-sha256` the
sha256 of its release tarball. The image workflow downloads the release
with the workflow's own token (`loomux/web` is public), refuses one whose `commit` doesn't match or whose
tarball doesn't hash to `deploy/web-sha256` (or whose `web-release.json`
says otherwise), and unpacks it into `.web-dist/`. The digest is pinned
here, not taken from the release, because a release can be edited after
the fact: `web-release.json` describes the bundle but isn't a trust
anchor. Every web bump is therefore a reviewed server PR changing both
lines; get the digest with
`gh release download web-<short> -R loomux/web -p web-release.json -O - | jq -r .sha256`
and check it against the tarball you download. A manual
`workflow_dispatch` with `web_ref` must pass `web_sha256` too. So the bundle in the image is the one `loomux/web`'s own CI
built and tested, byte for byte. The server build needs no Node toolchain,
and a commit without a release (anything pushed before LOOM-58) fails the
build loudly. The image build copies `web-release.json` into the bundle,
so loomuxd knows which release it serves; LOOM-118's in-app updater reads
the same file (see Configuration).

### The tradeoff being accepted

The image is **not self-contained**: `docker build .` on its own yields a
working JSON API with a placeholder page where the UI should be, not the
real client. The bundle's provenance lives in CI config rather than in the
Dockerfile. That is a deliberate trade — build-time credential safety and
reproducibility in exchange for the build not being one self-sufficient
command. The mitigation is that CI **must fail loudly** if the bundle is
missing, rather than falling through to the placeholder and shipping a
UI-less image that looks fine until someone opens it in a browser.

The placeholder page is not an error page: it states plainly that the image
was built without a web bundle and that the API under `/api/v1/` is fully
functional, so an operator who hits it knows immediately which half is
missing.

## Runtime layout

| Path | Contents |
| --- | --- |
| `/usr/local/bin/loomuxd` | The binary |
| `/srv/loomux/web` | Static web bundle (`LOOMUX_STATIC_DIR`) |
| `/var/lib/loomux` | Working directory; SQLite DB (`LOOMUX_DB_PATH`) |
| `/home/loomux` | `HOME`, mode 0700 — where `.ssh` goes (LOOM-52) |

### The runtime user is a fixed uid, deliberately

The image runs as **uid/gid 10001** (`loomux`), and unlike a pure-Go image
it **cannot** accept an arbitrary `runAsUser`. `ssh` calls `getpwuid()` and
refuses to run under a uid that has no `/etc/passwd` entry. A manifest that
sets `runAsUser: 1000` will start the container fine and then fail on the
first remote dispatch with `No user exists for uid 1000`.

So k8s should set `runAsUser: 10001` / `runAsGroup: 10001` /
`fsGroup: 10001`. Group-0 permissions are also applied to `$HOME` and
`/var/lib/loomux` so the image still behaves under an OpenShift-style
random-uid policy for everything *except* ssh.

### Persistence

`/var/lib/loomux` holds the SQLite database (workspaces, conversations,
sessions, the credential vault) and must be a PersistentVolume. Everything
else in the image is disposable.

`/tmp` must be writable: `targets/remote.go` puts its SSH ControlMaster
sockets under `${TMPDIR}/loomux/ssh-cm` (`os.MkdirAll(..., 0700)`), and
multiplexing fails without it. An `emptyDir` is fine — the sockets are not
worth persisting.

`$HOME/.ssh` must be writable as well, for the same reason `/tmp` is: the
entrypoint copies the SSH Secret there at startup. Under
`readOnlyRootFilesystem: true` that needs its own `emptyDir` or the pod
never starts. See `docs/deploy/ssh.md`.

Completion markers do **not** need a volume. `completion.MarkerWatcher`
checks for them through the *target's* executor, so for a remote target the
marker lives on the remote host, not in this container
(`router/router.go` sets `LOOMUX_MARKER_PATH` in the launched agent's
environment, and the agent's launch flags install the hook that touches
it; see `agents/README.md`). With `LOOMUX_MARKER_DIR` unset, each target
uses a per-user directory, `$HOME/.cache/loomux/completion-markers`.
Loomux creates it there as `0700` and refuses to use it if it's a
symlink or owned by someone else. A fixed `/tmp` path would be
predictable on a shared target: another user could create it first, so
turns hang, or plant markers that end turns early. If you set
`LOOMUX_MARKER_DIR`, it's used verbatim on every target and isn't
checked, so point it at a directory only the target user can write.

## Configuration

Every knob is an environment variable; the image sets sensible container
defaults for three of them.

**Required:**

| Variable | Notes |
| --- | --- |
| `LOOMUX_AUTH_PASSWORD_HASH` | bcrypt; generate with `loomuxd -hash-password`. Validated at startup — a malformed value is a hard failure |
| `LOOMUX_MASTER_KEY` | Credential-vault key. Its absence is only a *warning* at startup, and then every vault operation fails at runtime. Treat it as required |
| `LOOMUX_ROUTER_PRIMARY_BASE_URL` | Router model endpoint |
| `LOOMUX_ROUTER_PRIMARY_API_KEY` | |
| `LOOMUX_ROUTER_PRIMARY_MODEL` | |

**Set by the image:**

| Variable | Value |
| --- | --- |
| `HOME` | `/home/loomux` |
| `LOOMUX_HTTP_ADDR` | `:8080` |
| `LOOMUX_DB_PATH` | `/var/lib/loomux/loomux.db` (the default is relative to CWD, so setting it explicitly matters) |
| `LOOMUX_STATIC_DIR` | `/srv/loomux/web` |
| `LOOMUX_METRICS_ADDR` | `127.0.0.1:9090` (loopback only) |

**Optional:** `LOOMUX_SESSION_TTL` (720h, sliding), `LOOMUX_SESSION_MAX_AGE`
(2160h: a session's absolute lifetime; `0` for none), `LOOMUX_MARKER_DIR`,
`LOOMUX_REAP_IDLE_THRESHOLD` (24h), `LOOMUX_REAP_INTERVAL` (1h),
`LOOMUX_TARGET_PROBE_INTERVAL` (5m), `LOOMUX_TURN_RETENTION` (720h; `0` keeps
per-turn transcripts forever), `LOOMUX_EVENT_RETENTION` (2160h; `0` keeps the
dispatch audit trail forever), `LOOMUX_DISPATCH_MAX_DURATION` (2h: the
ceiling on one dispatch job end to end; each agent turn has its own,
tighter bounds inside it), `LOOMUX_DISPATCH_DRAIN` (20s: how long a
shutdown lets in-flight dispatch jobs finish before leaving them for the
next start to resume; then other HTTP requests get 5s more, so keep the
drain plus 5s under the pod's termination grace period, 30s by default), `LOOMUX_TMUX_SOCKET` (`loomux`: the tmux
socket every session runs on; two instances driving the same targets,
such as test and production, each need their own, or each one's orphan
sweep reaps the other's sessions), `LOOMUX_LOCAL_TARGETS` (`off` in this
image, `on` for a bare binary: whether targets of kind `local` may run;
see [targets.md](targets.md#local-targets)), `LOOMUX_SSH_PROXY`
(`socks5://host:port`, e.g. `socks5://127.0.0.1:1055` for a userspace
Tailscale sidecar: the proxy targets with a Loomux-managed SSH key are
reached through, relayed by loomuxd itself; see `docs/deploy/ssh.md`),
`LOOMUX_LOG_LEVEL` (`info`; JSON records on stderr for routing decisions,
provisioning and dispatch — see below), and the
`LOOMUX_ROUTER_ESCALATION_*` trio — all three or none, a partial set is a
startup error. `LOOMUX_AGENT_PROFILES` overrides how agents are launched
(permission and sandbox flags, workspace pre-trust, first prompt as an
argument). It's a JSON object keyed by agent type, e.g.
`{"claude-code":{"permission_args":["--permission-mode","plan"]}}`. A
malformed value, an unknown key, or an unknown agent type is a startup
error. Defaults and what each flag permits: `agents/README.md`. The
effective profile per agent type is logged at startup as
`agent launch profile`.

**Notifications (LOOM-102, optional):** `LOOMUX_NTFY_URL` and
`LOOMUX_NTFY_TOPIC` (both or neither) turn on an ntfy notification when a
turn ends: done, failed, or stopped on a prompt only you can answer
("needs you"). `LOOMUX_NTFY_TOKEN` authenticates to a protected topic (keep
it in the Secret). `LOOMUX_NOTIFY_EVENTS` narrows which ones are sent
(`done,failed,needs_you`); `LOOMUX_NOTIFY_MIN_DURATION` (30s) skips turns
quick enough that you were likely watching; `LOOMUX_PUBLIC_URL` (e.g.
`https://loomux.example`) makes each notification open its conversation.
At most 5 go out at once, then one a minute.

**Web client updates (LOOM-118, optional):** the web UI can switch the
web client it serves without rebuilding or redeploying the image.
`LOOMUX_WEB_UPDATES` chooses how:

- `attested` (the user's choice for the test instance): the newest
  `loomux/web` release that its CI built on `main`. A release is
  installed only if its GitHub build-provenance attestation verifies
  against Sigstore's public-good trust root (fetched over TUF, cached
  under `LOOMUX_WEB_BUNDLES_DIR/sigstore-tuf`), and only if:
  - the signing certificate was issued by GitHub Actions to
    `https://github.com/loomux/web/.github/workflows/ci.yml@refs/heads/main`,
    so no other branch, pull request or workflow qualifies;
  - the attestation's subject is the tarball's sha256, and the download
    must match it;
  - its SLSA provenance names the release's commit;
  - that commit is ahead, on `main`, of the one served, so updates never
    step back.
- `pinned`: what `loomux/server` main pins (`deploy/web-ref` +
  `deploy/web-sha256`), checked against that pin, so every UI change is a
  reviewed server PR.
- `off`: nothing is installed; the version served is still reported.

Unset means `attested` when `LOOMUX_WEB_RELEASES_TOKEN` is set (how the
feature was turned on before), otherwise `off`. **Production runs with
updates off** (user decision 2026-10-05): the UI changes only with the
image. Both repositories are public, so the token is optional; it only
raises GitHub's API rate limit (60 requests an hour without it, and a
check costs a few). `GET /api/v1/web/version` reports the bundle served
and what's on offer, `POST /api/v1/web/update` installs it, and
`POST /api/v1/web/rollback` returns to the bundle it replaced. All three
need a session. Installed bundles live in `LOOMUX_WEB_BUNDLES_DIR`
(default: `web-bundles` beside the database, on the data volume).

Whatever the mode, an update unpacks regular files and directories only,
into a fresh directory, and switches to it only once it's whole and has
an `index.html`. Deploying another image always starts from that image's
bundle. If the bundle manager can't start, loomuxd logs why and serves
the image's bundle without updates.

## Metrics (LOOM-103)

`loomuxd` exposes Prometheus metrics on `LOOMUX_METRICS_ADDR` (default
`127.0.0.1:9090`, path `/metrics`). The default binds to loopback only so
a standalone or host-networked container does not accidentally expose an
unauthenticated metrics endpoint on all interfaces. Set it explicitly to
`:9090` (all interfaces) when Prometheus scrapes it in-cluster via a
ServiceMonitor. An explicitly empty value (`LOOMUX_METRICS_ADDR=""`)
disables the endpoint entirely. The metrics carry no conversation/task IDs
or message content. Key series:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `loomux_dispatch_total` | `action`, `outcome`, `error_class` | Dispatch outcomes |
| `loomux_dispatch_stage_seconds` | `stage` | Latency per dispatch stage |
| `loomux_routing_decisions_total` | `action` | Router decisions |
| `loomux_router_calls_total` | `op`, `tier`, `outcome` | LLM calls |
| `loomux_router_call_seconds` | `op` | LLM call latency |
| `loomux_router_tokens_total` | `tier`, `kind` | Token consumption |
| `loomux_router_escalations_total` | `op` | Primary->escalation fallbacks |
| `loomux_router_retries_total` | `op`, `tier` | Corrective retries after an invalid tool call (LOOM-107) |
| `loomux_router_primary_breaker_open` | — | 1 while the primary tier is skipped after 3 consecutive failures (for 2 minutes; LOOM-107) |
| `loomux_tasks` | `status` | Task count by status |
| `loomux_target_up` | `target`, `kind` | Target reachability |
| `loomux_target_op_seconds` | `kind`, `op` | Target operation latency |
| `loomux_target_op_errors_total` | `kind`, `op`, `reason` | Failed target operations; an unreachable target's `reason` is `unreachable_<class>`, e.g. `unreachable_host_key_changed`, `unreachable_auth_failed`, `unreachable_proxy_unreachable` (LOOM-85) |
| `loomux_reaper_tasks_reaped_total` | — | Idle sessions torn down |

**What the logs contain.** Chat message bodies, direct answers and relayed
agent output are never logged (only a message's length). But treat the log
as sensitive anyway:

- **Error text is logged verbatim** on failure records. That includes SSH
  stderr from a remote target (`targets` wraps it into `ErrUnreachable`),
  which can name internal hosts, users and paths, and error bodies returned
  by the router-model provider's API.
- **`workspace_name` is chosen by the router model** for a new workspace,
  and can echo words from the user's message.

## Health probes (LOOM-105)

`GET /api/v1/health` is the cheap, unauthenticated liveness/readiness probe.
It pings the database and verifies the router model is configured, but it
does **not** probe targets or the Tailscale sidecar. Use it for Kubernetes
liveness and readiness. A database it can't reach makes it `unhealthy`; a
router model that isn't configured, `degraded`; both answer `503`. Since
it needs no session, a failed component's `error` is always the fixed
`unavailable`; the reason is in `/api/v1/health/deep`.

`GET /api/v1/health/deep` is authenticated and returns per-component detail:
database, router model, every registered target (via a short `tmux -V`
probe), and the sidecar SOCKS5 port. Use it for operational dashboards and
for debugging "why can't Loomux reach target X?". The target probes and the
sidecar dial run at once under one 10-second deadline; a target that hasn't
answered by then is reported `unhealthy` with the error `timed out`.

```yaml
livenessProbe:
  httpGet: { path: /api/v1/health, port: 8080 }
readinessProbe:
  httpGet: { path: /api/v1/health, port: 8080 }
```

`/api/v1/version` remains available for unauthenticated build identification
(CI stamps `version.Version` with the short commit SHA).

TLS is *not* terminated here. `loomuxd` serves plain HTTP by design; an
ingress or reverse proxy in front handles TLS (see `api/README.md`).

## SSH access to targets

See [`ssh.md`](ssh.md). In short: `$HOME/.ssh` is the entire
configuration surface for remote dispatch (production builds the remote
executor with no options), and it is populated at startup by
`deploy/entrypoint.sh`, which copies a read-only Secret mounted at
`/etc/loomux/ssh` into `$HOME/.ssh` at 0700/0600. The Secret cannot be
mounted at `~/.ssh` directly: a Secret volume is read-only, root-owned and
mode 0644, and `ssh` refuses a private key it considers group-readable.
That document also covers the SOCKS5 `ProxyCommand` reaching targets
through a userspace-mode Tailscale sidecar — which is what `nc`/`socat` in
this image are for.

## Verifying a build

A green `go build` is not evidence. Check the image itself:

```sh
docker run --rm --entrypoint /bin/sh ghcr.io/loomux/server -c \
  'ssh -V; tmux -V; nc -h 2>&1 | grep -- "-X proto"; \
   for b in sh ssh tmux nc socat; do command -v "$b" || echo "MISSING: $b"; done; \
   loomuxd -version'
```

Expect `ssh -V` and `tmux -V` to print versions, `nc -h` to show the `-X`
proxy-protocol flag (proving OpenBSD netcat rather than BusyBox), and
`loomuxd -version` to print the injected version rather than `dev`.
