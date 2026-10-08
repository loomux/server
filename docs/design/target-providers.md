# Target providers: dedicated agent environments on demand (LOOM-178)

**Status:** draft for approval (2026-10-08) · **Ticket:** LOOM-178 (Vikunja Loomux #145 / id 1475), the first step of
epic LOOM-177 (Loomux #144) · **Sub-issues it must leave startable:** LOOM-179 Kubernetes provider (#146),
LOOM-180 Docker provider (#147)
**Builds on:** LOOM-138 managed SSH keys, pins and the in-process agent (`docs/design/target-onboarding.md`,
merged as #280, #294, #301); LOOM-141 local targets off in the image; LOOM-114 host-key pinning; LOOM-89/90 target
policy and confined provisioning; LOOM-86 target health.

## Why

LOOM-141 turned `local` targets off in the container image: an agent on a local target runs as loomuxd's own
user and can read its database, the vault key and the managed SSH keys. The user still wants agents running on
the server's own infrastructure, without having to own a separate machine for them. A **provider** gives that
with real isolation: loomuxd creates a pod (Kubernetes) or a container (Docker) per target, with its own user,
filesystem, network identity and resource limits, and registers it as an ordinary remote target. Nothing in it can
reach loomuxd's secrets, and everything upstream of the target (orchestrator, completion markers, probes, the
orphan sweep, attach) works as it does for any other remote machine.

## What main has today (read at `397632d`)

- **Targets** are `local` or `remote` (`registry.Target`). A remote target is reached by exec'ing `ssh`
  (`targets/remote.go`), in one of two modes: `config` (the mounted `~/.ssh/config`) or **managed** (LOOM-138:
  a Loomux-generated ed25519 key served by a read-only in-process agent, `-F /dev/null`, a pin required before
  any connection, `LOOMUX_SSH_PROXY` relayed by loomuxd itself unless `ssh_proxy: none`). TEST already runs with
  managed targets (wyzer, jet01).
- **Workspaces** are directories under a target's `workspace_root` (`$HOME/loomux-workspaces` by default),
  provisioned by a Go-built POSIX script that confines the resolved path to the root (LOOM-90).
- **Agents** are launched per turn with everything on the command line (`agents/`, `router.launchAgent`);
  vault credentials are injected as env vars at launch; completion is a marker file under the target user's
  `$HOME/.cache/loomux/completion-markers`; Claude Code's folder trust is pre-set by a small Python script on the
  target, so `python3` must be there.
- **Health** (LOOM-86) probes every target on a timer (`tmux -V`, disk free, agent CLIs and their sign-in
  state); the **orphan sweeper** (LOOM-93) kills `loomux-*` sessions no task owns after 24 h.
- **The image** (`Dockerfile`, Alpine, uid 10001) is deployed by theWyseKube's manifests
  (`services/base/loomux`, `loomux-prod`): one Deployment with a userspace Tailscale sidecar (SOCKS5 on
  `127.0.0.1:1055`), a Ceph RBD PVC for the database, `fsGroup: 10001`, seccomp `RuntimeDefault`, no
  ServiceAccount of its own. The cluster is Talos Linux, Kubernetes v1.33, Ceph RBD (`ceph-rbd-sc`), Flux.
- **The API v1 contract** is additions-only from 1.0 (`api/testdata/api-v1-contract.txt`); `GET /targets/{id}`
  and `POST /workspaces` are listed as addable later (freeze review, section E).
- **Migrations** go up to `00025_tasks_conversation_index`; the next free number is `00026`.

## The model in one picture

```
  user ──► web: "Create a machine" ──► POST /api/v1/targets {provider: {...}}
                                              │
                                              ▼
                                   providers.Manager (in loomuxd)
                                   1. generate managed SSH key   (LOOM-138, as for any target)
                                   2. generate the environment's SSH host key
                                   3. provider.Create(spec)  ───────────────►  Kubernetes / Docker
                                      - pod or container from ghcr.io/loomux/agent
                                      - volume for /data (work + home)
                                      - authorized_keys + host key injected
                                   4. target row: kind remote, ssh_mode managed,
                                      host <env address>, port 2222, user agent,
                                      host key PINNED from step 2 (no scan, no TOFU)
                                   5. probe → ready
                                              │
          everything else unchanged ──────────┘
          (dispatch, provisioning, markers, health, sweep, attach-info, migrate-ssh N/A)
```

A provider-backed target is a **remote, managed target whose machine Loomux made**. The provider owns the
machine's lifecycle (create, start, stop, recreate, destroy, reconcile) and its connection fields; the rest of
Loomux treats it as any other target.

## 1. The provider interface and the plugin boundary

### 1.1 Interface

A new package `providers/` holds the interface, the `Manager` that composes it with the registry and the
targets package, a `fake` provider for tests, and one sub-package per real provider (`providers/kubernetes`,
`providers/docker`). The interface is deliberately plain data in and out: no callbacks, no Go-only types beyond
`context.Context` and `error`, so that an out-of-process adapter could implement it mechanically later (see 1.3).

```go
package providers

// Provider makes and manages environments: machines Loomux runs agents in.
// Every method is idempotent on its environment id, and safe to call again
// after a crash half-way: Create of an id that exists finishes what is
// missing and returns it; Destroy of an id that is gone returns nil.
type Provider interface {
    // Kind is "kubernetes", "docker" or "fake"; Name the configured instance ("wysekube", "jet01").
    Kind() string
    Name() string
    // Describe says what this provider can do right now: sizes, persistence, egress options, the agent
    // image it would use, and whether it is usable at all (and why not).
    Describe(ctx context.Context) (Info, error)
    // Create makes the environment spec describes, or finishes one half-made, and returns it. It returns
    // once the objects exist; readiness is reported by Get/Health as the environment starts.
    Create(ctx context.Context, spec EnvironmentSpec) (*Environment, error)
    Get(ctx context.Context, id string) (*Environment, error)
    // List returns every environment this Loomux instance owns at the provider (by label), whether or not
    // the registry knows it: this is what reconciliation and orphan cleanup read.
    List(ctx context.Context) ([]*Environment, error)
    // Stop keeps the environment's data and frees its compute; Start brings it back at the same address.
    Stop(ctx context.Context, id string) error
    Start(ctx context.Context, id string) error
    // Recreate replaces the running machine (a new image, a changed size) keeping its data and address.
    Recreate(ctx context.Context, id string, spec EnvironmentSpec) (*Environment, error)
    // Destroy removes everything, data included.
    Destroy(ctx context.Context, id string) error
    // Health is the provider's own view (phase, last event, restarts); SSH reachability is the target probe's.
    Health(ctx context.Context, id string) (Health, error)
    Close() error
}

type EnvironmentSpec struct {
    ID         string            // the environment id; every object name derives from it
    TargetID   string            // for labels and for a person reading the provider's objects
    Name       string            // the target's name, for labels/annotations only, never for object names
    Size       string            // one of Info.Sizes
    Persistent bool              // a volume that survives Stop/Recreate, or scratch that doesn't
    Egress     string            // "internet" (default) or "none"
    Image      string            // the agent image reference the provider was configured with
    SSH        SSHBootstrap      // what the machine needs to be reachable (see §2.2)
    Labels     map[string]string // instance id, target id, environment id
}

type SSHBootstrap struct {
    AuthorizedKey  string // the target's managed public key, one authorized_keys line
    HostPrivateKey []byte // the environment's sshd host key (ed25519, OpenSSH PEM), generated by the Manager
    HostPublicKey  string // its public half, what the target pins
    Port           int    // 2222: sshd runs as the agent user, so no privileged port
}

type Environment struct {
    ID          string
    Status      Status   // creating, starting, running, stopped, recreating, destroying, lost, error
    Reason      string   // plain language when Status is error or lost
    Address     Address  // how loomuxd reaches sshd: Host, Port, and whether through the server's proxy
    ImageDigest string   // what actually runs (for "update available")
    Size        string
    Persistent  bool
    Egress      string
    CreatedAt   time.Time
}

type Address struct {
    Host  string // a DNS name or IP that is stable for the environment's life
    Port  int
    Proxy string // the target's ssh_proxy: "none" (reached directly: in-cluster) or "default" (through LOOMUX_SSH_PROXY: a docker host on the tailnet)
}
```

### 1.2 The Manager: how an environment becomes a target

`providers.Manager` is the only code that touches both the registry and a provider. Creating a provider target:

1. Validate the request (name, provider, size, policy) and refuse if the provider isn't ready or its
   `max_environments` is reached.
2. Insert the **target row first**, in one transaction with the **environment row** (`environments` table,
   below): `kind: remote`, `ssh_key_ref` = a key generated for it (origin `target`, as `generate_ssh_key`
   does), `host`/`user` = the address the provider will give (deterministic, see 2.3; the Kubernetes port is
   2222 from the start, the Docker provider's published port is written back when `Create` returns and the
   target stays not ready until then), `ssh_proxy` per the provider, `host_keys` = the generated host public
   key **already pinned**, `workspace_root` = `/data/work`, environment status `creating`. The row exists
   before anything is made at the provider, so a crash leaves a `creating` row that startup resumes, never an
   unowned machine. Like every managed target this needs `LOOMUX_MASTER_KEY`: without it `POST /targets` with
   a `provider` answers `503`, as the managed-mode routes do.
3. In the background (a dispatch-style job on the server's own context, bounded by 10 minutes):
   `provider.Create`, then wait for `running`, then the normal target probe. Status moves
   `creating → starting → running`; a failure moves it to `error` with the provider's reason, and the objects
   are left for inspection (like a failed pane), to be destroyed by `DELETE` or retried by `POST .../recreate`.
4. From then on the target is a managed remote target: the executor, health, agent probes, provisioning and
   the orphan sweep need no change. The `migrate-ssh`, `scan-host-key`, `pin`/unpin and `generate_ssh_key`
   routes answer `409` for a provider target: its connection is the provider's to manage.

**Idempotency.** Every object a provider makes is named from the environment id (`lx-<id>` where id is a short
random base32 string, chosen so names fit Kubernetes' 63-character label and DNS rules and Docker's), never
from user text. `Create` is get-or-create per object, so a retry after a crash completes what is missing.
Target names are unique already (`409`), so a client retrying `POST /targets` can't make two machines.

**Reconcile on restart and on every probe.** At startup, and then on the target probe interval
(`LOOMUX_TARGET_PROBE_INTERVAL`, 5 min), the Manager compares the `environments` table with `provider.List`:

| Registry says | Provider says | Action |
|---|---|---|
| `creating` | partial or nothing | resume `Create` (idempotent) |
| `running` | running | nothing; the target probe decides readiness |
| `running` | gone (evicted node, deleted by hand) | persistent: `Start` again (same address, same data), log it; ephemeral: status `lost`, its workspaces `archived` with a `status_reason`, the user told on the Targets page |
| `stopped` | running | `Stop` (the registry is the intent) |
| `destroying` | anything | finish `Destroy`, then delete the rows |
| no row | objects with **this instance's** label | an orphan: logged, and destroyed once older than 24 h (the same TTL as the session orphan sweep) |

Objects without this instance's label are never touched, so a test and a production loomuxd sharing a cluster
can't reap each other's machines. The instance id is generated once and stored in the database
(a `settings` row, new), not configured: a restored backup keeps ownership of its machines, and a fresh database
on the same cluster can't claim them. Separate namespaces per instance (§10) are recommended on top.

### 1.3 Plugin boundary: compiled-in Go interface vs out-of-process plugin

| | Compiled-in (`providers.Provider` implementations in this module) | Out-of-process (hashicorp `go-plugin`-style gRPC, a binary per provider) |
|---|---|---|
| Deployment | the single binary the core design promises; nothing to install or version separately | plugin binaries in the image (or mounted), a protocol version to keep compatible, a handshake |
| Isolation | provider credentials (the ServiceAccount token, the docker host's SSH) in loomuxd's process, as the managed keys already are | the provider process could hold them instead, but in a pod the token is mounted into the container anyway, and the docker provider reuses loomuxd's own managed key: little gained for one user |
| Third-party providers | need a PR here | any language, no PR; nobody has asked for this |
| Testing | a fake provider is a struct; the Manager is tested in-process | a fake plugin process in CI, gRPC fixtures |
| Failure modes | a provider bug is loomuxd's | a crashed plugin is restartable; also a new way to be half-broken |
| Cost | small | the protocol, the plumbing, the release coupling |

**Recommendation: compiled-in.** Both providers in scope are ours, v1 is single-user, and the single-binary
deployment is a core-design decision. The interface above is kept serializable (plain structs, no callbacks)
precisely so that an out-of-process adapter is *one more implementation of `Provider`*, the same pattern the
core design uses for the deferred companion daemon behind `TargetExecutor`, not a redesign. Providers are
registered in `app.build` from configuration (§1.5); a kind that isn't configured isn't constructed.

### 1.4 Data model (migration `00026_environments`)

```sql
CREATE TABLE environments (
  id            TEXT PRIMARY KEY,            -- the short id object names derive from
  target_id     TEXT NOT NULL UNIQUE REFERENCES targets (id) ON DELETE RESTRICT,
  provider      TEXT NOT NULL,               -- configured provider name ("wysekube")
  provider_kind TEXT NOT NULL,               -- "kubernetes" | "docker"
  status        TEXT NOT NULL CHECK (status IN ('creating','starting','running','stopped',
                                                'recreating','destroying','lost','error')),
  status_reason TEXT NOT NULL DEFAULT '',
  size          TEXT NOT NULL,
  persistent    INTEGER NOT NULL,
  egress        TEXT NOT NULL,
  image         TEXT NOT NULL,               -- the reference it was created/recreated with
  image_digest  TEXT NOT NULL DEFAULT '',    -- what runs, once known
  host_key      TEXT NOT NULL,               -- the environment's host public key (the pin's source)
  host_private_key BLOB NOT NULL,            -- AES-256-GCM under the master key, AAD 'environment:'||id
  created_at    TIMESTAMP NOT NULL,
  updated_at    TIMESTAMP NOT NULL
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);   -- 'instance_id'
```

The host private key is kept so `Recreate`/`Start` can give a new pod the same host key, keeping the pin
valid for the environment's whole life. It is encrypted like the managed keys and never in a response. A
target with an `environments` row is a provider target; `targets` itself gains no column. Deleting the target
row requires the environment row gone first (`RESTRICT`), which the Manager's cascade (§5) does in order.

### 1.5 Configuration

Providers are configured at process start, like everything else, as one JSON object keyed by provider name
(the `LOOMUX_AGENT_PROFILES` convention). Unknown keys fail startup.

```json
LOOMUX_PROVIDERS='{
  "wysekube": {"kind": "kubernetes", "namespace": "loomux-agents", "storage_class": "ceph-rbd-sc",
               "subdomain": "loomux-agents", "agent_image": "ghcr.io/loomux/agent:0.4.0",
               "sizes": {"small":  {"cpu": "1", "memory": "2Gi", "disk": "10Gi"},
                         "medium": {"cpu": "2", "memory": "4Gi", "disk": "20Gi"}},
               "max_environments": 5},
  "jet01":    {"kind": "docker", "host_target": "jet01", "bind_address": "100.112.242.62",
               "agent_image": "ghcr.io/loomux/agent:0.4.0", "sizes": {"small": {"cpu": "1", "memory": "2Gi", "disk": "10Gi"}},
               "max_environments": 2}
}'
```

`agent_image` is required and explicit: the manifests pin the agent image as they pin the server image, and
"update available" means the configured image differs from the one an environment runs. Sizes are server-side
caps the API can only choose among, never exceed. With `LOOMUX_PROVIDERS` unset, `GET /providers` is an empty
list and the web UI doesn't offer "Create a machine".

## 2. From a provisioned environment to a target

### 2.1 Transport: in-pod sshd vs `kubectl exec`

| | sshd in the environment, reached through the existing managed-mode `RemoteExecutor` | a new `TargetExecutor` over the Kubernetes exec API (SPDY/WebSocket) and Docker's exec API |
|---|---|---|
| Code | none new on the executor side; the provider only injects a key and a host key | a third executor, with its own versions of the LOOM-84/85 work: deadlines, slots, failure classes, `RunOnce` stdin scripts, paste-buffer round trips |
| RBAC | `pods/exec` **not** needed: the most powerful namespaced verb stays ungranted | `pods/exec` on every agent pod: whoever holds the token can run anything in them |
| Load and latency | one multiplexed SSH connection per target, as today | every `send-keys`/`capture-pane` is an API-server exec session, audited and throttled by the API server |
| Two providers | identical path for Kubernetes and Docker | two exec APIs to implement and keep in step |
| Human attach | `kubectl exec -it ... tmux attach` or `ssh` via a port-forward; attach-info names both | the same |
| Cost | sshd (OpenSSH) in the agent image, ~10 MB; a port the NetworkPolicy must allow from loomuxd only | nothing in the image |

**Recommendation: sshd in the environment.** It makes a provider target a normal managed target, which is
the whole point: the executor hardening, health, the sweeper and `test` all apply unchanged, and LOOM-179 and
LOOM-180 share one connection story. The exec API is kept out of v1 entirely, so the ServiceAccount never needs
`pods/exec`.

### 2.2 Bootstrap: the key goes in, the host key comes from loomuxd

Two things make the first connection work without a scan, a TOFU step or a person:

- **authorized_keys**: the target's managed public key (generated as for any target, origin `target`, one key
  per environment) is handed to the provider in `SSHBootstrap.AuthorizedKey`, prefixed with
  `no-port-forwarding,no-agent-forwarding,no-X11-forwarding` (not `no-pty`: tmux needs one). No `from=`
  restriction: loomuxd's pod IP isn't stable, and the NetworkPolicy (§4) is what limits who reaches port 2222.
- **The host key is generated by the Manager**, not by sshd on first boot: ed25519, private half given to the
  provider to place in the environment, public half **pinned on the target row before the machine exists**. So
  managed mode's "no pin, no connection" rule is satisfied from the start, there is no window in which a
  different machine could answer, and `Start`/`Recreate` reuse the same host key so the pin never changes.
  `scan-host-key`/`pin` answer `409` for a provider target, with the reason.

How the two files reach the machine is provider-specific (§8, §9); in both the image's entrypoint copies them
into a private 0700 directory with 0600 modes before starting sshd, the same pattern as `deploy/entrypoint.sh`
uses for `~/.ssh` (a Secret volume is root-owned 0644, and sshd's `StrictModes` wants the key private).

### 2.3 Addressing

- **Kubernetes**: the pod sets `hostname: lx-<id>` and `subdomain: <subdomain>`; one headless Service named
  `<subdomain>` (created once by the manifests, selector `loomux.io/role=agent`) makes
  `lx-<id>.<subdomain>.<namespace>.svc.cluster.local` resolve to the pod's current IP. The target's `host` is
  that name (it passes the managed-mode host grammar), `ssh_port` 2222, `ssh_proxy: none` (in-cluster; the
  Tailscale sidecar isn't in the path). A restarted pod gets a new IP and the same name, and the same host key,
  so nothing on the target row changes.
- **Docker**: the container publishes 2222 on the docker host's configured `bind_address` (its tailnet IP) with
  an ephemeral host port read back from `inspect`; the target's `host` is the docker host target's host,
  `ssh_port` that port, `ssh_proxy` the docker host target's (`default` through the sidecar from the cluster).
  When loomuxd runs as a bare binary on the docker host itself, `bind_address` may be `127.0.0.1`.

### 2.4 Workspaces: one environment per target, one volume per environment

**An environment is a target, not a workspace.** The registry's model (a workspace is a directory on a target;
the router picks a target when it provisions) stays as it is, and a machine made for "project X" simply gets its
first workspace provisioned there the usual way. A per-workspace environment (pod per workspace, with the
target 1:1 to it) is a later mode if wanted, not v1: it would need the router to create machines and a
different cost model (one PVC per repository).

Each environment has one volume mounted at `/data`:

| Path | Role | Why one volume |
|---|---|---|
| `/data/work` | the target's `workspace_root`; workspaces are directories under it, confined as today | one PVC per environment keeps Ceph's object count and the quota simple |
| `/data/home` | `$HOME` of the agent user: `~/.cache/loomux/completion-markers`, `~/.claude` (`CLAUDE_CONFIG_DIR=/data/home/.claude` so `.claude.json` — account, folder trust — lands in the volume too), `~/.codex`, git config | a one-time interactive sign-in and the folder trust survive restarts and image updates |

`persistent: true` (default) backs `/data` with a PVC (Kubernetes) or a named volume (Docker), sized by the
chosen size's `disk`; `Stop` frees the pod/container and keeps it; `Recreate` (a new image, another size)
keeps it; `Destroy` deletes it. `persistent: false` backs `/data` with an `emptyDir` / anonymous volume: cheap
scratch for throwaway work, gone with the machine; `Stop` isn't offered for it (it would be a destroy), and a
lost ephemeral machine archives its workspaces with the reason. In both cases `/` is read-only, `/tmp` is a
tmpfs (tmux's socket and the paste buffers live there), and the health probe's disk-free number is `/data`'s.

The existing workspace rules hold unchanged: `DELETE /workspaces/{id}` never touches files; a `failed`
workspace is kept for inspection; the 1 GiB free rule applies to `/data`.

## 3. The agent image (`ghcr.io/loomux/agent`)

Built from this repo (`deploy/agent/Dockerfile`, `deploy/agent/entrypoint.sh`, `deploy/agent/sshd_config`) by
a workflow of its own (`agent-image.yml`) on the same version line as the server: tag `<version>` for a
release, `main` for a tested main build, plus the digest. The manifests pin the tag or digest in
`LOOMUX_PROVIDERS.*.agent_image`, exactly as they pin the server image.

**Contents.** Debian slim (glibc: the agent CLIs ship native binaries), Node.js 22 LTS (Claude Code requires 22
or later; Codex is an npm package too), `@anthropic-ai/claude-code@<pinned>`, `@openai/codex@<pinned>`,
`opencode-ai@<pinned>` (each pinned to an exact version, above the floors in `agents/README.md`, with
`DISABLE_AUTOUPDATER=1` so a container never drifts from what was tested), `tmux`, `openssh-server`, `git`,
`python3` (the Claude Code folder-trust script), `ripgrep`, `curl`, `ca-certificates`, `tzdata`, `jq`, and a
minimal build toolchain only if a size budget allows it (open question 8). No Docker CLI, no kubectl, no
cloud CLIs: an agent that needs one asks for it in chat and the install offer runs in its pane like on any
target.

**User.** `agent`, uid/gid 10002, `$HOME=/data/home`. Distinct from the server's 10001 so a misconfigured
volume can never be read across. The image also sets `CLAUDE_CONFIG_DIR=/data/home/.claude`.

**sshd.** Runs as `agent` (not root: no privilege separation user, no setuid), `Port 2222`,
`HostKey /run/loomux/ssh/host_ed25519` (copied in by the entrypoint from wherever the provider put it, 0600),
`AuthorizedKeysFile /run/loomux/ssh/authorized_keys` (likewise), `PasswordAuthentication no`,
`KbdInteractiveAuthentication no`, `PubkeyAuthentication yes`, `AllowUsers agent`, `AllowTcpForwarding no`,
`AllowAgentForwarding no`, `X11Forwarding no`, `PermitUserEnvironment no`, `UseDNS no`, `LogLevel VERBOSE`
to stderr (the pod log is the only place a failed login is visible). The entrypoint ends with `exec sshd -D -e
-f /etc/loomux/sshd_config` so the container's process 1 is sshd and its exit is the pod's.

**Agent auth gets in two ways, never a third.**

1. **Vault credentials at launch**, as on every target: `credentials.ShellEnvPrefix` puts the resolved
   values in the pane's environment. For a machine Loomux made this is the natural path, so the vault gains a
   **target scope**: `POST /credentials` accepts `target_id` (optional, like `workspace_id`/`agent_type`),
   and the precedence becomes workspace+agent > workspace > target+agent > target > agent > global. The user
   stores `ANTHROPIC_API_KEY` or a `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`, a one-year token for
   subscription plans), `OPENAI_API_KEY`, a `GITHUB_TOKEN`, scoped to the machine, and every agent launched
   there has them. Checked 2026-10-08 against code.claude.com: `ANTHROPIC_API_KEY` is used in interactive
   mode after a one-time in-pane confirmation, `CLAUDE_CODE_OAUTH_TOKEN` takes precedence over stored
   credentials; the exact env names for Codex and opencode are confirmed in LOOM-179's image smoke test, not
   assumed here.
2. **A one-time interactive sign-in** kept in `/data/home`: attach (`kubectl exec`), run `claude` or
   `codex login`, finish the browser flow with the paste-code fallback. The `login_required` error class
   (LOOM-97) already leaves the pane for exactly this. With `CLAUDE_CONFIG_DIR` in the volume the sign-in
   survives restarts and image updates.

What never gets in: any `LOOMUX_*` variable, the master key, the managed private keys, a ServiceAccount
token, the docker socket. The pod's environment is only what the image sets; the pane's environment is the
image's plus the vault values for that workspace, agent and target.

**Updates.** The environment records the image reference and digest it runs. `GET /providers` reports the
configured image; a target whose digest differs shows `update_available: true`, and `POST
/targets/{id}/recreate` replaces the machine with the configured image, keeping `/data` and the address. It is
refused (`409`) while a task is mid-turn or taken over. There is no in-place `npm update`: a version change is
an image change, reviewed as a server PR bumping the pin, like a web bump.

## 4. Security and threat model

The single-user assumption of v1 holds: everything a provider makes belongs to the one user, and the routes
below join the "admin-only once there are roles" list the freeze review keeps (section D).

| Asset / threat | Mitigation |
|---|---|
| **Provider credentials** (the ServiceAccount token = make/delete pods in one namespace; the docker host's SSH = root-equivalent on that host, since the docker group is) | Kubernetes: a dedicated ServiceAccount `loomuxd` bound to a **Role in the agents namespace only** (pods, PVCs, Secrets: create/get/list/watch/delete; `pods/log` get; **no `pods/exec`, no `services`, nothing cluster-wide, nothing in the `loomux` namespace**). Docker: the engine is reached over SSH to a registered, managed, pinned target, with the same key hygiene as every target; loomuxd never holds a docker TLS client cert |
| **The docker socket inside an agent container** | never mounted; the provider talks to the engine from loomuxd, and the agent image has no docker client. Agent containers get `--cap-drop ALL`, `--security-opt no-new-privileges`, a non-root user, `--read-only`, `--pids-limit` |
| **An agent reaching loomuxd** (its API, its database PVC, its SOCKS sidecar) | agents live in **another namespace** (`loomux-agents`) with a default-deny ingress NetworkPolicy (only loomuxd's pod may reach 2222) and an egress policy that allows DNS and the internet but **denies the cluster CIDRs and RFC 1918** (`ipBlock` with `except`), so a pod can't reach the API server, loomuxd, Ceph or the LAN. Egress `none` cuts the internet too. **Caveat:** NetworkPolicy is only enforced by a CNI that implements it; theWyseKube's CNI must be confirmed in the manifests handoff (§10). On Docker there is no NetworkPolicy: a user-defined bridge with `enable_icc=false` isolates containers from each other, `--internal` is egress `none`, and the host's LAN stays reachable from an `internet` container. That limitation is documented, not hidden |
| **An agent reaching another agent's machine** | default-deny ingress between pods in the namespace; `enable_icc=false` on Docker |
| **An agent escaping the container** | Pod Security Admission `restricted` on the namespace: `runAsNonRoot`, seccomp `RuntimeDefault`, all capabilities dropped, no privilege escalation, read-only root, no host namespaces or host paths; the provider's pod spec is written to pass `restricted` and CI asserts it. A sandboxed runtime (gVisor/Kata `RuntimeClass`) isn't in theWyseKube today and is left as a later option (open question 12) |
| **Resource exhaustion** (a runaway build, a fork bomb, a full disk) | requests and limits from the chosen size, `pids` limit, PVC size; a `ResourceQuota` and `LimitRange` on the namespace cap the total; `max_environments` per provider caps the count; the 1 GiB free rule keeps provisioning off a full volume |
| **Secret exposure** | the managed private key stays in loomuxd (in-process agent, LOOM-138); the environment's host private key is stored encrypted in the registry and placed in the environment via a Secret that only the agents-namespace Role can read, mounted into only that pod; the `authorized_keys` line is public material. Vault values reach the pane's environment at launch, as on every target, and are visible to that agent by design, never written to the volume by Loomux. Pod names, labels and annotations carry ids and the target name, never hosts, users or key material |
| **A compromised agent image** (supply chain of three npm packages) | exact version pins, `DISABLE_AUTOUPDATER`, the image rebuilt only by a reviewed PR, pinned by digest in the manifests, scanned in CI (`govulncheck` doesn't apply; `trivy` on the image is proposed) |
| **Name/argument injection** (user text into object names, ssh argv) | object names derive from random ids only; the target's host is the Manager's deterministic DNS name and passes the managed-mode grammar; the user's name is a label value, validated against Kubernetes' label grammar or dropped to an annotation |
| **Who may create, stop, destroy machines** | every route is behind `requireAuth`; with roles these are admin routes |
| **Lost master key** | provider targets fail like any managed target (unreadable key); environments are destroyed and recreated. Documented in backup-restore |
| **A second Loomux instance on the same cluster** | instance id label on every object and `List` filtered by it; separate namespaces recommended |

**Multi-tenant assumptions.** One user, one namespace, one quota. Nothing in this design assumes otherwise,
and nothing prevents a later per-user namespace: the Manager keys everything by instance id, a user id would
be one more label and one more namespace parameter.

## 5. API additions (additive to v1)

All additions; nothing existing changes shape or meaning. Each is recorded in the contract golden file with
`-update` in its PR.

**Providers**

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/providers` | `{providers: [{name, kind, ready, error, agent_image, sizes: [{name, cpu, memory, disk}], persistent_default, egress_options: ["internet","none"], max_environments, environments}]}`. Empty when none is configured |

**Targets**

- `POST /api/v1/targets` accepts a new optional object `provider: {name, size, persistent, egress}`. With it,
  `kind` must be `remote` or omitted, and `host`, `user`, `ssh_port`, `ssh_key_id`, `generate_ssh_key`,
  `ssh_proxy`, `workspace_root` must be absent (`400` naming the field): the provider sets them. The policy
  fields work as before. Answer: `201` with the target, `provider.status: creating`; creation continues in the
  background.
- Target responses gain `provider` (null for an ordinary target):
  `{name, kind, environment_id, status, status_reason, size, persistent, egress, image, image_digest,
  update_available, created_at}`. `ready`/`next_step` keep their meaning; `next_step` stays null while the
  machine is coming up, and `provider.status` says why the target isn't ready.
- `GET /api/v1/targets/{id}` (new; listed as addable in the freeze review), so the web can poll one target
  while it is created.
- `PUT /api/v1/targets/{id}` on a provider target refuses (`400`) the connection fields it refuses on create;
  name, `permission_mode`, policy and `relay` stay editable.
- `POST /api/v1/targets/{id}/stop`, `POST /api/v1/targets/{id}/start`,
  `POST /api/v1/targets/{id}/recreate` (body `{size?}`; the image is always the configured one): `202` with
  the target; `409` while a task there is mid-turn or taken over, `409` for `stop` of an ephemeral machine,
  `404`/`409` for a target without a provider.
- `DELETE /api/v1/targets/{id}` on a provider target **cascades**: refused (`409`, listing what blocks) while
  any task is mid-turn or taken over; otherwise it deletes the target's workspaces with their tasks (as
  `DELETE /workspaces/{id}` does, one transaction), its target-scoped credentials, destroys the environment
  (data included), deletes the generated key and the rows. For an ordinary target the existing rule (refuse
  while workspaces exist) is unchanged.
- `scan-host-key`, `pin`, `DELETE .../pin`, `migrate-ssh` answer `409` on a provider target.

**Credentials**: `POST /api/v1/credentials` accepts `target_id`; `GET` lists it; the uniqueness rule becomes one
name per (workspace, target, agent_type) scope.

**Attach-info**: gains `attach_commands: [{via, command}]` beside the unchanged `attach_command`: for a
Kubernetes target `via: "kubectl"`, `kubectl -n <ns> exec -it <pod> -- tmux -L loomux attach -t <session>`
(the pod's DNS name isn't resolvable from a laptop; this is); for Docker `via: "docker"`, `ssh <host> docker
exec -it <container> tmux -L loomux attach -t <session>`; `via: "ssh"` with the plain form for every target.

**Health and metrics**: `GET /health/deep` gains a `providers` component (each provider's `Describe`);
`loomux_environments{provider,status}` and `loomux_provider_ops_total{provider,op,outcome}` are exported.
Lifecycle steps are logged as structured records (`environment create`, with ids and the provider's message,
never key material).

## 6. Web UX: "Create a machine" (loomux/web, separate PR)

Targets page → **Add target** opens a two-way choice:

- **Connect a machine** — the LOOM-138 wizard (host, user, verify host key, authorize, test), unchanged.
- **Create a machine** — shown only when `GET /providers` returns a ready provider. Fields: provider (hidden
  when there is one), name, size (cards with cpu/memory/disk), **Keep data** toggle (persistent, on by
  default, with one line saying what off means), egress (Internet / None), and the usual policy block
  collapsed (purpose, permission mode, allowed agents, confirmation, relay). **Create** posts and the new card
  appears at once with a progress strip driven by `GET /targets/{id}`: Creating → Starting → Ready, or an
  error with the provider's reason and a **Retry** (recreate) button.

A provider target's card carries a provider badge (`wysekube · medium · keeps data`), the usual health line,
and actions **Stop**/**Start**, **Update** (only when `update_available`, with the configured image named),
**Delete** (a confirmation listing the workspaces and saying the data goes with it). The attach panel shows the
`kubectl`/`docker` command from attach-info first, the ssh form second. A stopped machine's workspaces are
shown greyed with "machine stopped"; a dispatch to one starts it first (§7).

## 7. The router's view of providers

The routing model sees a provider target as it sees any target: id, name, kind `remote`, agents, problem,
policy (`TargetSnapshot`), plus two new fields: `Provider` (the provider kind, or empty) and `Ephemeral`.
The prompt says what they mean: "a machine Loomux created; only its own workspaces are there; an ephemeral one
loses its files when it is destroyed". Policy defaults for provider targets are the permissive ones (purpose
personal, provision and shell allowed, no confirmation), since nothing else runs there, and the user can
tighten them in the form.

**Dispatch to a stopped machine.** A `use_workspace`/`provision_workspace`/`run_command` aimed at a target
whose environment is `stopped` starts it first (a dispatch stage, bounded like a provisioning run, shown in the
audit trail as an event) and then goes on; `creating`/`starting` waits for readiness up to the same bound;
`error`/`lost` fails the dispatch with `target_unhealthy` and the reason. The snapshot's `Problem` says
"stopped; will be started" so the model doesn't avoid the target.

**No machine creation by the router in v1.** `provision_target` as a routing action (the model asking for a
new machine, behind a require-confirmation offer with the size and cost shown, like a clone it wasn't told
about) is a natural follow-up once a provider has been used by hand for a while. Until then machines are made
from the Targets page or `POST /targets`, and a request that needs one gets the LOOM-68 style direct answer
naming the page.

## 8. Kubernetes provider (LOOM-179)

**Client.** A small typed REST client over `net/http` with the in-cluster ServiceAccount (token file, CA,
`KUBERNETES_SERVICE_HOST`), or a kubeconfig path for tests (`LOOMUX_TEST_KUBECONFIG`). It needs six verbs on
three resource kinds; `k8s.io/client-go` would add a few hundred modules to `go.sum` and `govulncheck`'s
surface for that. The structs are hand-written for the fields used (open question 6 keeps client-go as the
alternative if the hand-written client grows past, say, a thousand lines).

**Objects per environment**, all labelled `app.kubernetes.io/managed-by=loomuxd`,
`loomux.io/instance=<instance id>`, `loomux.io/target=<target id>`, `loomux.io/environment=<env id>`,
`loomux.io/role=agent`, `loomux.io/egress=internet|none` (what the NetworkPolicies select on), and annotated
`loomux.io/target-name=<name>`:

| Object | Name | Notes |
|---|---|---|
| Secret | `lx-<id>-ssh` | `host_ed25519` (private), `authorized_keys`; `type: Opaque`; mounted read-only at `/etc/loomux/ssh-src`, copied by the entrypoint |
| PersistentVolumeClaim | `lx-<id>-data` | `ReadWriteOnce`, the configured `storage_class`, the size's `disk`; only for `persistent: true` |
| Pod | `lx-<id>` | below |

**The pod spec** (what CI asserts, field by field):

```yaml
metadata: {name: lx-<id>, labels: {...}, annotations: {loomux.io/target-name: <name>}}
spec:
  hostname: lx-<id>
  subdomain: <subdomain>                       # with the headless Service: a stable DNS name
  automountServiceAccountToken: false
  enableServiceLinks: false                    # no *_SERVICE_HOST env telling the agent about the cluster
  restartPolicy: Always                        # sshd exits → kubelet restarts it; data stays
  securityContext: {runAsNonRoot: true, runAsUser: 10002, runAsGroup: 10002, fsGroup: 10002,
                    seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: agent
    image: <agent_image>                       # by digest once known
    env: [{name: HOME, value: /data/home}, {name: CLAUDE_CONFIG_DIR, value: /data/home/.claude}, {name: TZ, value: <server's>}]
    ports: [{name: ssh, containerPort: 2222}]
    resources: {requests: {cpu: <size>, memory: <size>}, limits: {cpu: <size>, memory: <size>, ephemeral-storage: 2Gi}}
    securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
    readinessProbe: {tcpSocket: {port: ssh}, periodSeconds: 5}
    volumeMounts:
    - {name: data, mountPath: /data}
    - {name: tmp, mountPath: /tmp}
    - {name: run, mountPath: /run/loomux}
    - {name: ssh-src, mountPath: /etc/loomux/ssh-src, readOnly: true}
  volumes:
  - name: data; persistentVolumeClaim: {claimName: lx-<id>-data}   # or emptyDir: {sizeLimit: <disk>} when ephemeral
  - {name: tmp, emptyDir: {sizeLimit: 1Gi}}
  - {name: run, emptyDir: {medium: Memory, sizeLimit: 1Mi}}
  - {name: ssh-src, secret: {secretName: lx-<id>-ssh, defaultMode: 0400}}
```

This passes Pod Security `restricted`. There is no init container and no `pods/exec`: the entrypoint does the
copy, and the Manager learns the pod is up from `status.phase`/`containerStatuses` and then from the target
probe over SSH.

**Lifecycle mapping.** `Create` = Secret, PVC, Pod (get-or-create each). `Stop` = delete the Pod (Secret and
PVC stay). `Start` = create the Pod again. `Recreate` = delete the Pod, create it with the new image/size.
`Destroy` = delete Pod, PVC, Secret (each tolerating 404). `Health` = phase, container state, restart count,
the last event message on failure (`ImagePullBackOff`, `Pending` on a PVC that can't bind, an evicted pod),
reported in `status_reason` in the provider's words. Bare Pods rather than a StatefulSet/Deployment: loomuxd
is the controller already (it reconciles on every probe), there are no `apps` RBAC verbs to grant, and a
one-replica StatefulSet would still need the same volume and address handling (open question 5).

**Image pull.** The agent image is public on GHCR, like the server's; no `imagePullSecrets`. A private image
would be one more Secret the Role can read and `imagePullSecrets` on the pod.

**Ready signal.** `running` once the pod is `Running` with the container ready (tcp 2222 answering); the
target probe (`ssh` + `tmux -V`) then sets `ready`. Both are needed: a pod can be `Running` with sshd refusing
the key (a copy error), which the `test` steps show as `auth` failed, with the pod log named in the hint.

## 9. Docker provider (LOOM-180)

**Transport.** The Engine API (HTTP over a stream) through **SSH to a registered target** named in the
provider's `host_target`: loomuxd runs `ssh <managed argv> -- user@host docker system dial-stdio` and speaks
HTTP over the process's stdio (the same thing the Docker CLI's `ssh://` contexts do). The ssh argv is the
managed-mode builder's, so the key, the pin, the proxy and the hardening are the ones every target already
has; nothing new reaches the host. The `host_target` must be a managed, pinned, ready remote target, and its
user must be in the docker group (documented: that is root on that host). A second transport, the local unix
socket (`"socket": "/var/run/docker.sock"`), is for a bare-binary loomuxd on the docker host itself. No TLS
TCP endpoint in v1.

**Objects per environment**, labelled like the pods (`loomux.io/*` labels on Docker objects):

| Object | Name | Notes |
|---|---|---|
| Network | `loomux-agents` (shared, created once per host) | user-defined bridge, `com.docker.network.bridge.enable_icc=false` (no container-to-container); a second, `--internal` network `loomux-agents-internal` backs egress `none` |
| Volume | `lx-<id>-data` | only for `persistent: true`; an anonymous volume otherwise |
| Container | `lx-<id>` | below |

**The container**: image `<agent_image>`, `--user 10002:10002`, `--read-only`, `--tmpfs /tmp:size=1g`,
`--tmpfs /run/loomux:size=1m`, `--cap-drop ALL`, `--security-opt no-new-privileges:true`,
`--security-opt seccomp=default`, `--pids-limit 512`, `--memory`/`--cpus` from the size, `--mount
type=volume,source=lx-<id>-data,target=/data`, `--network loomux-agents`, `--publish
<bind_address>:0:2222` (an ephemeral host port, read back with `inspect`), `--restart unless-stopped`,
env `HOME`, `CLAUDE_CONFIG_DIR`, `TZ` as for the pod, labels as above. Nothing from the host is mounted;
`/var/run/docker.sock` is never passed.

**Bootstrap without Secrets.** Docker (outside Swarm) has no Secret object. After `create` and before `start`,
the provider uploads a tar with `host_ed25519` (0600) and `authorized_keys` into `/etc/loomux/ssh-src` with
`PUT /containers/{id}/archive`; the entrypoint copies them to `/run/loomux/ssh` (tmpfs) exactly as in the pod.
The uploaded files live in the container's writable layer, which `--read-only` makes immutable at run time
and which is deleted with the container; a `Recreate` uploads them again.

**Lifecycle mapping.** `Create` = network (get-or-create), volume, container, archive upload, start. `Stop` =
`stop` (the container and its published port stay; the port number is fixed on first create and recorded, so
the target's `ssh_port` never changes). `Start` = `start`. `Recreate` = `rm` the container, create again with
the same name, port and volume. `Destroy` = `rm -f`, remove the volume. `Health` = `inspect` state, exit code,
restart count. `List` = containers by label. Orphan cleanup = containers/volumes with this instance's label and
no row.

**What Docker can't give**, documented with the provider: no NetworkPolicy (the LAN is reachable from an
`internet` container; `none` is the `--internal` network), no quota beyond per-container limits (so
`max_environments` matters more), no PSA (the provider asserts its own flags instead), and the docker host's
own security is the user's.

## 10. What theWyseKube would need (handoff to command-center; nothing edited here)

For the TEST instance (`services/base/loomux`), mirrored later for `loomux-prod` with its own namespace:

1. **Namespace** `loomux-agents`, labelled `pod-security.kubernetes.io/enforce: restricted` (and
   `warn`/`audit` the same), `app.kubernetes.io/part-of: loomux`.
2. **ServiceAccount** `loomuxd` in `loomux` (`automountServiceAccountToken: true`), set as
   `serviceAccountName` on the Deployment; a **Role** in `loomux-agents`:
   `pods` (create, get, list, watch, delete), `pods/log` (get), `persistentvolumeclaims` (create, get, list,
   delete), `secrets` (create, get, list, delete), `events` (list, for `status_reason`); a **RoleBinding** to
   the ServiceAccount. No ClusterRole, no `pods/exec`, nothing in `loomux` itself.
3. **Headless Service** `loomux-agents` in `loomux-agents`: `clusterIP: None`, selector
   `loomux.io/role: agent`, port 2222; `publishNotReadyAddresses: true` so the name resolves while sshd starts.
4. **NetworkPolicy** in `loomux-agents`: (a) default deny ingress and egress for `loomux.io/role=agent`;
   (b) allow ingress to 2222 from `namespaceSelector: {name: loomux}` + `podSelector: {app: loomuxd}`;
   (c) allow egress UDP/TCP 53 to kube-dns; (d) allow egress to `0.0.0.0/0` **except** `10.244.0.0/16`,
   `10.96.0.0/12`, `192.168.0.0/16`, `172.16.0.0/12`, `10.0.0.0/8` — for `egress: internet` pods (label
   `loomux.io/egress=internet`; `none` pods match only (a)–(c)). **To confirm first:** which CNI the Talos
   cluster runs and whether it enforces NetworkPolicy (Talos' default Flannel doesn't; Cilium/Calico do). If it
   doesn't, the policies are documentation until it does, and that changes the threat model's "agent reaching
   loomuxd" row: then the agents namespace should at least not share a node pool with loomuxd's data, and the
   handoff should say so plainly.
5. **ResourceQuota** `loomux-agents` (TEST, to tune): `requests.cpu: 4`, `limits.cpu: 8`,
   `requests.memory: 8Gi`, `limits.memory: 16Gi`, `pods: 6`, `persistentvolumeclaims: 6`,
   `ceph-rbd-sc.storageclass.storage.k8s.io/requests.storage: 100Gi`; a **LimitRange** with the `small`
   defaults so a pod never runs unbounded.
6. **loomuxd Deployment**: `LOOMUX_PROVIDERS` in `loomuxd-config` (the JSON in §1.5, no secrets in it);
   if loomuxd itself ever gets a NetworkPolicy, allow its egress to `loomux-agents` on 2222.
7. **Image pin** `ghcr.io/loomux/agent@sha256:…` in `LOOMUX_PROVIDERS`, bumped like the server image.
8. **PBS backup**: `lx-*-data` PVCs are the user's working copies of repositories, not system state; recommend
   **not** adding them to the nightly PBS set in v1 (git remotes are the backup), stated in the handoff so it
   is a decision, not an omission.
9. **Prod**: nothing until TEST has run a provider for a while; then `loomux-prod-agents` with the same shape.

## 11. Phased PR plan

| PR | Scope | Depends on |
|---|---|---|
| **1. Providers core** (`providers/`, LOOM-178 implementation) | the interface, `fake` provider, `Manager` (create/start/stop/recreate/destroy, reconcile, orphans, instance id), migration `00026`, `LOOMUX_PROVIDERS` parsing, `GET /providers`, target `provider` fields and lifecycle routes, `GET /targets/{id}`, cascade delete, vault `target_id` scope, attach-info `attach_commands`, router snapshot fields and the start-if-stopped dispatch stage, metrics, deep-health component, `core-design.md` §1/§7 updated, docs (`docs/deploy/providers.md`). Everything tested against the fake | LOOM-138 (merged) |
| **2. Agent image** | `deploy/agent/{Dockerfile,entrypoint.sh,sshd_config}`, `agent-image.yml` (build, smoke test, trivy, publish `ghcr.io/loomux/agent`), `docs/deploy/agent-image.md` | none (can run beside PR 1) |
| **3. Kubernetes provider** (LOOM-179) | `providers/kubernetes`: REST client, objects, pod spec, health; `kind`-based CI job; handoff for §10 | PR 1, PR 2 |
| **4. Docker provider** (LOOM-180) | `providers/docker`: ssh dial-stdio and socket transports, objects, archive upload; CI against the runner's docker | PR 1, PR 2 |
| **5. Web** (loomux/web) | "Create a machine" flow, provider cards, lifecycle actions, attach commands, update badge | PR 1 (fake provider for dev) |
| **6. Deploy** | theWyseKube changes landed by command-center; TEST creates its first machine; a dispatch round-trip; then the user decides about prod | PR 3, §10 |
| later | `provision_target` routing action; per-workspace environments; sandboxed runtime class; private agent images | use |

PR 1 is the big one and is where the API additions land; it ships with no real provider, so it changes
nothing for a deployment without `LOOMUX_PROVIDERS`.

## 12. Test strategy

- **Unit, in-process (every PR).** `providers/fake`: an in-memory provider with scriptable failures (create
  fails at step 2, pod vanishes, image pull hangs) so the Manager's state machine, reconcile table, orphan TTL,
  cascade delete and resume-after-crash are tested without a cluster; API contract additions via
  `TestAPIv1Contract -update`; negative tests: provider fields on a plain target, connection fields on a
  provider target, sizes not in the list, `stop` of an ephemeral machine, `pin`/`migrate-ssh` on a provider
  target, a second instance's objects never listed.
- **Kubernetes provider.** (a) Unit tests against an `httptest` server replaying the API (create/get/list/delete
  bodies, 404/409 paths, a Pending pod with an event), asserting every security field of the pod spec and the
  labels. (b) An **integration job in CI** (`k8s-integration`, separate from `test`, required for provider
  PRs): `helm/kind-action` brings up kind, the job builds the agent image and `kind load`s it, applies the §10
  manifests (a test copy under `deploy/test/kind/`), and a `//go:build k8sintegration` test creates an
  environment through the real provider, waits for `running`, port-forwards 2222, and connects through the real
  managed-mode `RemoteExecutor` (AgentPool, pinned host key with the forwarded `[127.0.0.1]:port` host) to run
  `tmux -V` and the `test` steps, then `Stop`/`Start`/`Destroy` and asserts nothing is left. kind enforces no
  NetworkPolicy by default; the policies are validated by `kubectl apply --dry-run=server` only.
- **Docker provider.** Unit tests against an `httptest` Engine API; an integration test on the GitHub runner's
  own docker over the unix socket (create, archive upload, ssh in via the published port on `127.0.0.1`, tmux,
  destroy). The ssh dial-stdio transport is exercised against `targets/sshtest` extended with a command handler
  that pipes to the local socket; the first real run is against jet01 by hand (the user's machine), recorded in
  the PR.
- **Agent image.** The workflow builds it, runs `claude --version`, `codex --version`, `opencode --version`
  (above the floors), `tmux -V`, `sshd -t`, checks the user is 10002 and `/` is read-only, generates a key,
  starts the container with it and logs in over ssh, runs the folder-trust Python script once, and scans with
  trivy (fail on critical).
- **Security tests** (the LOOM-139 pattern): the pod spec and the container flags carry no `LOOMUX_*`, no
  token automount, no socket mount; a JSON-response scan for the host private key and the managed private key
  on every provider route; logs captured for key material; label values from user text never reach object
  names.
- **On TEST**: create a small machine, provision a workspace by chat, run a Claude Code turn with a target-scoped
  token, stop/start, update after an image bump, destroy; all four `test` steps green at each stage.

## 13. Open questions, each with a recommendation

1. **Plugin boundary** — compiled-in `Provider` interface *(recommended)* vs out-of-process gRPC plugins. §1.3.
2. **Transport** — sshd in the environment behind the managed-mode executor *(recommended)* vs a Kubernetes/
   Docker exec executor. §2.1.
3. **Environment granularity** — one environment per target, workspaces as directories on it *(recommended)*
   vs one per workspace. §2.4.
4. **Namespace** — a separate `loomux-agents` namespace per instance, PSA `restricted` *(recommended)* vs
   agents in `loomux` itself (simpler RBAC, no isolation from loomuxd's secrets: rejected).
5. **Pod controller** — bare Pods reconciled by loomuxd *(recommended)* vs a one-replica StatefulSet per
   environment (stable name and volume for free, but `apps` RBAC, scale-to-zero semantics and a second
   reconciler).
6. **Kubernetes client** — a hand-written REST client over `net/http` *(recommended)* vs `client-go`.
7. **Host key** — generated by loomuxd and injected, pinned before first contact *(recommended)* vs generated
   by sshd and read back (needs `pods/log` parsing or exec, and a TOFU window).
8. **Agent image base and contents** — Debian slim + Node 22, the three CLIs pinned, python3, git, ripgrep,
   no compilers *(recommended; a `-full` variant with build tools can follow if agents keep asking for them)*.
9. **Agent auth** — vault with a new `target_id` scope, plus a persistent home for a one-time interactive
   sign-in *(recommended both)* vs only one of them.
10. **Default egress** — internet allowed, cluster and RFC 1918 denied *(recommended)* vs deny-all by default
    (agents can't clone or install anything without an explicit choice).
11. **Docker transport** — the Engine API over `docker system dial-stdio` through a registered managed
    target *(recommended)* vs a TLS TCP endpoint with client certs (one more credential kind to store).
12. **Sandboxed runtime** — not in v1 *(recommended)*; a `runtime_class` provider setting is a one-line
    addition when theWyseKube has gVisor or Kata.
13. **Router creating machines** — not in v1 *(recommended)*; `provision_target` behind a confirmation later.
14. **Deleting a provider target** — cascade (workspaces, tasks, credentials, data) in one `DELETE`
    *(recommended)* vs keeping the rows and marking the environment destroyed (two steps, history kept).
15. **PVC backups** — not in the nightly PBS set *(recommended)*; git remotes are the backup.
16. **Agent image provenance** — plain GHCR tag + digest pin *(recommended for v1)* vs Sigstore attestation
    like the web bundle (LOOM-118 already has the verifier; a follow-up once the image is stable).

## 14. Decisions for the user

Approving this doc with the recommendations above means: compiled-in providers (1), sshd-in-environment on the
managed executor (2), environment per target with one `/data` volume (3), a separate PSA-restricted namespace
(4), bare pods (5), a hand-written Kubernetes client (6), injected host keys (7), the image as in §3 (8), vault
target scope plus persistent home (9), internet-only egress by default (10), Docker over ssh dial-stdio (11), no
sandbox runtime, no router-made machines (12, 13), cascading delete (14), no PVC backups (15), tag + digest
pinning (16). Anything the user decides differently is changed here before PR 1 starts, and the change is
sent back to command-center as a handoff.
