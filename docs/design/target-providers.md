# Target providers as plugins: dedicated agent machines on demand (LOOM-178)

**Status:** revised draft for approval, 2026-10-08 (revision 2: a plugin system, per the user's feedback
through command-center; revision 1 is in this file's history) · **Ticket:** LOOM-178 (Vikunja Loomux #145 /
id 1475), the first step of epic LOOM-177 (Loomux #144) · **Sub-issues it must leave startable:** LOOM-179
Kubernetes plugin (#146), LOOM-180 Docker plugin (#147)
**Builds on:** LOOM-138 managed SSH keys, pins and the in-process agent (`docs/design/target-onboarding.md`,
merged as #280, #294, #301); LOOM-141 local targets off in the image; LOOM-114 host-key pinning; LOOM-89/90
target policy and confined provisioning; LOOM-86 target health; LOOM-118's Sigstore verifier.

## Why

A target is a machine Loomux runs agents on. There are two ways to get one:

1. **Register a host you already use**: a laptop, a PC, a VPS, a work server. This is the LOOM-138 path
   (scan the host key, pin it, authorize the Loomux key, test) and it stays the primary, plugin-free way
   most targets come to exist. Nothing in this document changes it.
2. **Create a machine on the infrastructure Loomux itself runs on**, for work the user doesn't want on their
   own devices, and as the replacement for the `local` target LOOM-141 turned off in the image (an agent
   there runs as loomuxd's own user and can read its database, vault key and SSH keys). This needs
   infrastructure credentials and infrastructure-specific code, so it is **opt-in per deployment, as a
   plugin**: a Kubernetes plugin makes a pod per target, a Docker plugin a container per target; later ones
   can make Proxmox or libvirt VMs, or cloud VPSs. A created machine has its own user, filesystem, network
   identity and resource limits, and is registered as an ordinary managed remote target, so everything
   upstream of the target (orchestrator, completion markers, probes, the orphan sweep, attach) is unchanged.

This document designs the plugin system (§1), what a created machine is and how it becomes a target (§2),
the agent image that runs in it (§3), the threat model (§4), the API and UI (§5–§7), the two first-party
plugins (§8, §9), what theWyseKube needs (§10), the PR plan and tests (§11, §12), and the open questions
with recommendations (§13, §14).

## What main has today (read at `397632d`, rebased onto `f1f7b74`)

- **Targets** are `local` or `remote` (`registry.Target`). A remote target is reached by exec'ing `ssh`
  (`targets/remote.go`), in one of two modes: `config` (the mounted `~/.ssh/config`) or **managed** (LOOM-138:
  a Loomux-generated ed25519 key served by a read-only in-process agent, `-F /dev/null`, a pin required before
  any connection, `LOOMUX_SSH_PROXY` relayed by loomuxd itself unless `ssh_proxy: none`). TEST already runs
  managed targets (wyzer, jet01).
- **Workspaces** are directories under a target's `workspace_root` (`$HOME/loomux-workspaces` by default),
  provisioned by a Go-built POSIX script that confines the resolved path to the root (LOOM-90, LOOM-153).
- **Agents** are launched per turn with everything on the command line (`agents/`, `router.launchAgent`);
  vault credentials are injected as env vars at launch; completion is a marker file under the target user's
  `$HOME/.cache/loomux/completion-markers`; Claude Code's folder trust is pre-set by a small Python script on
  the target, so `python3` must be there.
- **Health** (LOOM-86) probes every target on a timer (`tmux -V`, disk free, agent CLIs and their sign-in
  state); the **orphan sweeper** (LOOM-93) kills `loomux-*` sessions no task owns after 24 h.
- **Secrets at rest**: the vault's values and the managed SSH private keys are AES-256-GCM under
  `LOOMUX_MASTER_KEY` with the row id as additional data (`registry/sqlite/crypto.go`); the API never returns
  them. LOOM-185 added the router's own provider keys, set and rotated from Settings and stored the same
  way. Plugin configuration secrets follow the same rule.
- **Signed artifacts**: LOOM-118 verifies the web bundle's GitHub build-provenance attestation against
  Sigstore's trust root (`webbundle/attest.go`, `sigstore-go`), pinned to one workflow identity. The same
  verifier applies to plugin artifacts.
- **The image** (`Dockerfile`, Alpine, uid 10001) is deployed by theWyseKube's manifests
  (`services/base/loomux`, `loomux-prod`): one Deployment with a userspace Tailscale sidecar (SOCKS5 on
  `127.0.0.1:1055`), a Ceph RBD PVC for the database, `fsGroup: 10001`, seccomp `RuntimeDefault`, no
  ServiceAccount of its own. The cluster is Talos Linux, Kubernetes v1.33, Ceph RBD (`ceph-rbd-sc`), Flux.
  theWyseKube is checking which CNI it runs and whether NetworkPolicy is enforced; command-center forwards
  the answer (it affects §4 and recommendation 12).
- **The API v1 contract** is additions-only from 1.0 (`api/testdata/api-v1-contract.txt`); `GET
  /targets/{id}` and `POST /workspaces` are listed as addable later (freeze review, section E).
- **Migrations** go up to `00027_router_settings`; the next free number is `00028`.

## The model in one picture

```
 Machines (targets)
 ├── registered hosts ─── LOOM-138 wizard (scan, pin, authorize, test) ─────────────── plugin-free
 └── created machines ─── a target-provider plugin: create / start / stop / recreate / destroy
                                      │
   loomuxd, the HOST ◄── JSON-RPC over stdio or a unix socket ──► the PLUGIN process (kubernetes, docker, …)
   owns every registry row; generates the managed key          holds the infrastructure credentials;
   and the machine's host key; pins it before contact;          makes the pod / container from the agent
   validates everything the plugin says; probes over SSH        image; injects authorized_keys + host key
                                      │
   the machine: sshd on 2222 as user "agent", /data for work and home
   ─► an ordinary managed remote target: dispatch, markers, health, sweep, attach — all unchanged
```

## 1. The plugin system

### 1.1 What a plugin is

A plugin is a **separate program** loomuxd starts or connects to, described by a **manifest**, speaking a
**versioned protocol** (§1.3). The host drives it; a plugin never calls into loomuxd (no "give me a secret",
no database access), so the host's trust boundary is the protocol's input validation plus what the plugin was
given: its own configuration, and the bootstrap material for one machine at a time. In v1 the only capability
a plugin can declare is **target provider** (`targets.*`, §1.9); the framework is capability-agnostic so later
ones (a notifier, a credential source) reuse it without a second plugin mechanism.

First-party plugins live in this repo (`plugins/kubernetes`, `plugins/docker`), are built, tested, signed and
published with the server, and are **bundled in the loomuxd image** so installing one is a product action,
not a deployment change. A third-party plugin is any program with a manifest that speaks the protocol,
placed where the host looks (§1.8).

Principles the rest of this section follows:

- The host owns all registry state (targets, workspaces, environments); the plugin owns infrastructure
  objects, labelled with the instance id so it only ever touches its own.
- A plugin receives only what it needs: its configuration at start, the bootstrap material of one machine on
  `targets.create`. Never the vault, never the managed private keys, never another machine's material.
- Everything a plugin returns is untrusted input: addresses must pass the managed-mode host grammar, statuses
  must be from the fixed list, strings are bounded and shown as plain text.
- Credentials for the infrastructure belong to the plugin, stored by the host encrypted (§1.5) and handed
  over on the private channel at start; in the sidecar form (§1.8) they are mounted into the plugin's
  container only and the host never sees them.

### 1.2 Packaging and distribution: the decision

| | **A.** compiled into loomuxd, behind an enable switch | **B.** out-of-process, first-party binaries bundled in the image, run as subprocesses | **C.** out-of-process as a sidecar container (its own image), reached over a unix socket | **D.** downloaded from a registry at install time |
|---|---|---|---|---|
| "Install" in the UI | a switch; the code is always there | a real action: review the manifest, configure, start | configure and connect; the container was added to the deployment | fetch, verify, then as B |
| Infrastructure credentials | in loomuxd's process | in the plugin process, same container and uid as loomuxd (readable by a same-uid process: no boundary) | in the plugin container only (the ServiceAccount token projected there; loomuxd's container has none) | as B |
| Third-party plugins | a PR to this repo | drop a directory on the plugin path | add a container to the deployment | the registry |
| loomuxd's dependency graph | gains client-go / the Docker API | unchanged (each plugin is its own module) | unchanged | unchanged |
| A plugin crash or hang | loomuxd's | isolated, supervised, restarted | isolated; kubelet restarts it | as B |
| Deployment change to adopt | none | none | a sidecar container, a shared socket volume, the token projection | none, plus a registry and a fetch policy |
| Cost | lowest | the protocol, a supervisor, a manifest, bundling | B plus one manifest change per deployment | B plus fetching, caching, a registry format |

**Recommendation: B and C together, with one protocol.** Plugins are out-of-process from v1. First-party
plugins ship three ways from the same build: bundled as binaries in the loomuxd image (so a single-container
or bare-binary deployment installs them from the UI with no deployment change), as signed release assets
(for a bare binary that wants a newer plugin than its image), and as their own container images
(`ghcr.io/loomux/plugin-kubernetes`, `ghcr.io/loomux/plugin-docker`) for the sidecar form. **On Kubernetes the
sidecar form is the recommended production shape**, and the one theWyseKube should use (§10): it is the only
form in which loomuxd's container never holds the ServiceAccount token, so a compromise of loomuxd's HTTP
surface cannot be turned into arbitrary Kubernetes API calls, only into the protocol's own operations, which
the plugin confines to its labelled objects.

**A is rejected for v1**: its "install" would be a lie, it gives no credential boundary at all, every provider
grows the server's dependency graph and `govulncheck` surface, and a third-party plugin needs a PR here.
**D is deferred**: the host verifies and runs what the operator put on disk or beside it; fetching from a
registry is a later feature, and the signature verification it needs is designed now (§1.6).

The single-binary promise of the core design becomes a **single-image** promise: `ghcr.io/loomux/server`
carries loomuxd and its first-party plugins. A bare `loomuxd` binary works without any plugin, which is the
plugin-free registered-host path.

### 1.3 The protocol: `loomux-plugin/1`

**JSON-RPC 2.0** with `Content-Length` framing (the LSP/MCP convention), over the subprocess's stdio or a
unix socket. Chosen over gRPC because: the host needs no code generation toolchain and no gRPC dependency;
the calls are request/response with polling (a create takes minutes and is modelled as an immediate return
plus `targets.get`), so streaming buys nothing yet; a fake plugin for tests is a tiny program, even a shell
script; and it is the same shape as the MCP servers this ecosystem already runs, so a third-party author
knows it. gRPC (or hashicorp `go-plugin`) would be `loomux-plugin/2` if streaming or non-stdio transports are
ever needed; the manifest names the protocol, so both can coexist.

Methods the host calls (every one has a per-call deadline, 30 s unless noted):

| Method | Purpose |
|---|---|
| `plugin.describe()` → manifest | the handshake; the host refuses a plugin whose manifest differs from the one on disk or whose protocol major isn't `1` |
| `plugin.configure({config, host: {version, instance_id, data_dir}})` | the plugin's configuration, secrets included (the channel is private to the two processes), plus what it needs to label its objects; called at every start and after a config change |
| `plugin.check()` → `{ok, problems: [{code, message}]}` | credentials and reachability: can it list pods in its namespace, does the docker engine answer |
| `plugin.shutdown()` | stop cleanly; the host kills after 10 s |
| `targets.describe()` → `{sizes, persistent_default, egress_options, image, max_environments, environments}` | what the UI offers; may change with configuration |
| `targets.create(spec)` → environment | returns at once with status `creating` or `starting`; idempotent on `spec.id` (§1.9) |
| `targets.get(id)`, `targets.list()` | `list` returns every environment carrying this instance's label, whether or not the host knows it (reconcile, orphans) |
| `targets.start(id)`, `targets.stop(id)`, `targets.recreate(id, spec)`, `targets.destroy(id)` | lifecycle; each idempotent and safe to repeat after a crash (2 min) |
| `targets.health(id)` → `{status, reason, restarts, image_digest}` | the plugin's own view; SSH reachability is the host's probe |
| `targets.attach_commands(id, session)` → `[{via, command}]` | the `kubectl exec` / `docker exec` form of attach for a person |

A plugin may send the notification `targets.changed {id, status}` on the same channel; the host treats it as
a hint to poll sooner and never as the truth. Errors are JSON-RPC errors with a `code` from a fixed list
(`invalid_config`, `unauthorized`, `not_found`, `quota`, `unavailable`, `internal`) and a `message` safe to
show; the host maps them to its own error classes. Anything else (unknown method, malformed frame, a field
outside its grammar) fails the call and counts against the plugin's health.

**Versioning.** The manifest's `protocol` major must equal the host's; a minor the host doesn't know is
tolerated (additions only, the same rule as API v1). `min_host_version` lets a plugin refuse an old host.
Each environment records the plugin version that made it, so an upgrade that can't adopt old environments
can say so instead of breaking them.

### 1.4 The manifest: capabilities, permissions, configuration schema

`plugin.json` beside the executable (or returned by `plugin.describe` for a sidecar):

```jsonc
{
  "name": "kubernetes", "title": "Kubernetes", "version": "0.4.0", "protocol": "loomux-plugin/1",
  "vendor": "Loomux", "homepage": "https://github.com/Loomux/server/tree/main/plugins/kubernetes",
  "description": "Creates a pod per machine in one namespace of the cluster Loomux runs in.",
  "min_host_version": "0.4.0",
  "capabilities": ["targets.create", "targets.stop_start", "targets.recreate", "targets.persistent",
                   "targets.ephemeral", "targets.egress_policy", "targets.attach_commands"],
  "permissions": [                                  // what the plugin needs on the infrastructure
    {"scope": "kubernetes", "detail": "create, get, list, watch and delete Pods, PersistentVolumeClaims and Secrets in one namespace; read Pod logs and Events there"},
    {"scope": "network",    "detail": "reaches the Kubernetes API server and the image registry"}
  ],
  "host_requests": [],                              // what it asks of the host: always empty in v1
  "config_schema": {                                // a JSON Schema subset the UI renders as a form
    "type": "object", "required": ["namespace", "storage_class", "agent_image"],
    "properties": {
      "namespace":     {"type": "string", "default": "loomux-agents", "description": "where machines are made; must exist, with the plugin's Role"},
      "storage_class": {"type": "string", "default": "ceph-rbd-sc-delete", "description": "a Delete-reclaim class: a Retain class leaves volumes behind, and the quota allows none of it"},
      "subdomain":     {"type": "string", "default": "loomux-agents", "description": "the headless Service that names machines"},
      "agent_image":   {"type": "string", "x-format": "image-reference"},
      "kubeconfig":    {"type": "string", "x-secret": true, "x-format": "kubeconfig", "description": "leave empty in-cluster (the mounted ServiceAccount is used)"},
      "sizes":         {"type": "object", "additionalProperties": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}, "disk": {"type": "string"}}}},
      "max_environments": {"type": "integer", "minimum": 1, "default": 5}
    }
  }
}
```

- **Capabilities** map to UI affordances and host behaviour: no `targets.stop_start`, no Stop button; no
  `targets.ephemeral`, no "Keep data" toggle. The host refuses a method a plugin didn't declare.
- **Permissions** are human-readable declarations for informed consent, shown before install (§6). The
  host cannot enforce what a plugin does with infrastructure credentials, so they are not a sandbox; they
  are what the user agrees to. `host_requests` is the place a future capability would declare what it
  needs from the host, and the host would enforce that; in v1 it must be empty.
- **`config_schema`** is a JSON Schema subset (`object`, `string`, `integer`, `boolean`, `enum`, `default`,
  `required`, one level of nested objects) the web renders as a form. `x-secret: true` fields are stored
  encrypted, never returned, and shown as "set / not set"; `x-format` gives the UI a widget: `ssh-private-key`
  (with a **Generate** button: the host makes an ed25519 key, stores it as the secret and shows the public
  half for the user's `authorized_keys`), `kubeconfig`, `image-reference`, `socks5-url`.
- **Sizes and the image** are also reported at runtime by `targets.describe`, so a plugin can derive them from
  its configuration rather than hard-code them in the manifest.

### 1.5 Lifecycle in the product

| Step | API | What happens |
|---|---|---|
| **Discover** | `GET /api/v1/plugins/available` | the catalog: every manifest found in the bundle directory, the operator's plugin directory, and every sidecar socket (§1.8), with `source` (`bundled`, `dir`, `socket`), `trust` (§1.6), capabilities, permissions, config schema, and whether it is already installed |
| **Install** | `POST /api/v1/plugins {plugin, label, config}` | validates `config` against the schema, encrypts `x-secret` fields (AES-256-GCM under the master key, AAD `plugin:<id>:<field>`, like the LOOM-138/LOOM-185 keys; `503` without a master key), writes the row `installing`, starts or connects to the plugin, runs `describe` → `configure` → `check`; `installed` (or `error` with the problems, the row kept so the config can be fixed). A plugin may be installed more than once with different configuration (two Docker hosts); `label` names the instance and is unique |
| **Configure** | `GET /api/v1/plugins/{id}`, `PUT /api/v1/plugins/{id}/config` | the stored config with secrets as `{"set": true}`; a `PUT` with a secret omitted keeps the stored value, with `""` clears it; then `configure` + `check` again, and `targets.describe` is refreshed |
| **Disable / enable** | `POST /api/v1/plugins/{id}/disable`, `.../enable` | disable stops the process (or disconnects). Its targets **keep working**: they are ordinary managed SSH targets; what stops is lifecycle control (start/stop/recreate/destroy), reconcile and orphan cleanup, and the UI says "plugin disabled" on each. Enable starts it again and reconciles |
| **Upgrade** | `GET /api/v1/plugins` shows `installed_version` and `available_version`; `POST /api/v1/plugins/{id}/upgrade` | a bundled plugin's available version changes with the image; a directory plugin's when the operator replaces it; a sidecar's when its container changes. Upgrade starts the new version beside the old one, runs its handshake and `check`, and only then stops the old one and writes the new version to the row. If the new one can't start or fails its check, it is stopped, **the old version keeps running and the row keeps its version**, and the call answers `422` with the reason. It is refused up front (`409`) if the new manifest drops a capability an environment relies on or its protocol major is unknown. Environments record `plugin_version`; the plugin may refuse to adopt ones from a version it can't read, naming them |
| **Uninstall** | `DELETE /api/v1/plugins/{id}?targets=destroy\|keep` | `409` listing its targets when the parameter is absent and it has any. `destroy`: each target is deleted with the cascade of §5 (the machines included) and then the plugin stops. `keep`: the targets stay as registered managed targets (their environment rows keep the history, with the plugin reference cleared and a reason), the machines keep running wherever they are until someone removes them by hand, and the UI marks them "created by a removed plugin". Then the process stops and the stored secrets are erased |
| **Startup** | — | every `installed` and enabled plugin is started or connected; one that fails goes to `error` with the reason and its targets show it; the rest of loomuxd is unaffected. Reconcile (§1.9) runs once each plugin is up |

Routes join the "admin-only once there are roles" list (freeze review, section D).

### 1.6 Trust model

| Plugin origin | How it is trusted | What the UI shows before install |
|---|---|---|
| **Bundled first-party** (in the loomuxd image) | the same supply chain as loomuxd itself: built by this repo's CI from a reviewed commit, the image pinned by digest in the manifests | "Part of this Loomux release", version, capabilities, permissions |
| **First-party as a release asset or its own image** | signed in CI with GitHub build-provenance attestations; verified at install with the LOOM-118 verifier (`webbundle/attest.go`'s pattern) against Sigstore's trust root, with the identity pinned to `github.com/loomux/server/.github/workflows/plugins.yml@refs/tags/v*` (release) or `@refs/heads/main` (TEST, like attested web updates) | "Signed by Loomux CI", the commit, version, capabilities, permissions |
| **Third-party** (a directory on the plugin path, or a sidecar) | unsigned, or signed by another identity the host can verify but doesn't know | "Not signed by Loomux" (or "Signed by `<identity>`"), the permissions, and an explicit confirmation: "I trust this plugin with the permissions above" |

Stated plainly in that confirmation, because it is true: a plugin run as a **subprocess runs as loomuxd's
own user**, so it can read loomuxd's database file and process environment; the subprocess boundary isolates
crashes and dependencies, not secrets. The form that limits a third-party plugin is the **sidecar
container**: its own user, its own mounts, only the socket shared. The UI's third-party warning says so and
recommends it, and `GET /plugins/available` marks subprocess third-party plugins `trust: "unsigned"` with
`isolation: "none"`.

A **target-provider plugin is trusted with everything that runs on the machines it makes**: it receives the
machine's host private key to inject (§2.2), so a malicious one could stand up an impostor machine that the
host would connect to and hand agent credentials to. No protocol rule can remove that; it is why install is
explicit, why permissions are shown, and why first-party plugins are the ones bundled.

### 1.7 Data model (migration `00028_plugins`)

```sql
CREATE TABLE plugins (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,                 -- manifest name ("kubernetes")
  label          TEXT NOT NULL UNIQUE,          -- the instance's name ("wysekube", "jet01")
  version        TEXT NOT NULL,                 -- installed manifest version
  protocol       TEXT NOT NULL,                 -- "loomux-plugin/1"
  source         TEXT NOT NULL CHECK (source IN ('bundled','dir','socket')),
  path           TEXT NOT NULL,                 -- directory or socket path
  trust          TEXT NOT NULL,                 -- 'bundled' | 'signed:<identity>' | 'unsigned'
  status         TEXT NOT NULL CHECK (status IN ('installing','installed','disabled','error')),
  status_reason  TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  config         BLOB NOT NULL,                 -- JSON; x-secret fields AES-256-GCM, AAD 'plugin:'||id||':'||field
  capabilities   TEXT NOT NULL DEFAULT '[]',    -- JSON array, as installed (upgrade compares)
  installed_at   TIMESTAMP NOT NULL,
  updated_at     TIMESTAMP NOT NULL
);

CREATE TABLE environments (
  id               TEXT PRIMARY KEY,            -- the short id object names derive from
  target_id        TEXT NOT NULL UNIQUE REFERENCES targets (id) ON DELETE RESTRICT,
  plugin_id        TEXT REFERENCES plugins (id) ON DELETE RESTRICT,   -- NULL after an uninstall with targets=keep
  plugin_name      TEXT NOT NULL,               -- kept for the "created by" badge after an uninstall
  plugin_version   TEXT NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('creating','starting','running','stopped',
                                                   'recreating','destroying','lost','error','detached')),
  status_reason    TEXT NOT NULL DEFAULT '',
  size             TEXT NOT NULL,
  persistent       INTEGER NOT NULL,
  egress           TEXT NOT NULL,
  image            TEXT NOT NULL,               -- the reference it was created/recreated with
  image_digest     TEXT NOT NULL DEFAULT '',    -- what runs, once known
  host_key         TEXT NOT NULL,               -- the machine's host public key (the pin's source)
  host_private_key BLOB NOT NULL,               -- AES-256-GCM under the master key, AAD 'environment:'||id
  created_at       TIMESTAMP NOT NULL,
  updated_at       TIMESTAMP NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);   -- 'instance_id'
```

The host private key is kept so `start`/`recreate` can give a new pod the same host key, keeping the pin
valid for the machine's whole life; it is encrypted like the managed keys and never in a response. A target
with an `environments` row is a created machine; `targets` itself gains no column. `detached` is the status
after an uninstall with `targets=keep`.

### 1.8 Deployment forms and discovery

| Form | Where the host looks | How it runs | Configuration |
|---|---|---|---|
| **Bundled subprocess** | `LOOMUX_PLUGIN_BUNDLE_DIR` (`/usr/local/lib/loomux/plugins`, set by the image): one directory per plugin with `plugin.json` and the executable | spawned on install/enable with a clean environment (`PATH`, `HOME`, `TMPDIR`, the locale; never `LOOMUX_*`), stdio as the channel, stderr to loomuxd's log prefixed with the plugin label; supervised: restarted with backoff, `error` after three failures in five minutes | from the host at `plugin.configure` |
| **Operator-installed subprocess** | `LOOMUX_PLUGIN_DIR` (default `plugins/` beside the database, on the data volume) | the same | the same |
| **Sidecar / socket** | `LOOMUX_PLUGIN_SOCKET_DIR` (`/run/loomux/plugins`): one `<name>.sock` per plugin, served by a container in the same pod (or a process on the same host) | never spawned; the host connects, `plugin.describe` supplies the manifest; a lost socket is `unavailable` until it is back | from the host at `plugin.configure`, except credentials the container already has mounted (the ServiceAccount token), which its schema marks optional |

All three appear in the catalog with their `source`; the same plugin name from two sources is two catalog
entries, and the sidecar one is preferred by the UI. The protocol and the manifest are identical across
forms: a first-party plugin is one `main` that serves stdio when started with no arguments and a socket when
started with `--listen <path>`.

### 1.9 The target-provider capability: how a machine becomes a target

`providers.Manager` (package `providers/`, host side) is the only code that touches both the registry and a
plugin's `targets.*` methods. Creating a machine:

1. Validate the request (name, plugin instance, size, policy) and refuse if the plugin isn't `installed`
   and enabled, its `check` isn't ok, or its `max_environments` is reached.
2. Insert the **target row first**, in one transaction with the **environment row**: `kind: remote`,
   `ssh_key_ref` = a key generated for it (origin `target`, as `generate_ssh_key` does), `host`/`user` = the
   address the plugin will give (deterministic, see 2.3; the Kubernetes port is 2222 from the start, the
   Docker plugin's published port is written back when `create` returns, and the target stays not ready until
   then), `ssh_proxy` per the plugin's `targets.describe`, `host_keys` = the generated host public key
   **already pinned**, `workspace_root` = `/data/work`, environment status `creating`. The row exists before
   anything is made, so a crash leaves a `creating` row that startup resumes, never an unowned machine. Like
   every managed target this needs `LOOMUX_MASTER_KEY`: without it `POST /targets` with a `plugin` answers
   `503`, as the managed-mode routes do.
3. In the background (a dispatch-style job on the server's own context, bounded by 10 minutes):
   `targets.create`, then poll `targets.get` until `running`, then the normal target probe. Status moves
   `creating → starting → running`; a failure moves it to `error` with the plugin's reason, and the objects
   are left for inspection (like a failed pane), to be destroyed by `DELETE` or retried by `recreate`.
4. From then on the target is a managed remote target: the executor, health, agent probes, provisioning and
   the orphan sweep need no change. The `migrate-ssh`, `scan-host-key`, `pin`/unpin and `generate_ssh_key`
   routes answer `409` for a created machine: its connection is the plugin's to manage.

**Idempotency.** Every object a plugin makes is named from the environment id (`lx-<id>` where id is a short
random base32 string, chosen so names fit Kubernetes' 63-character label and DNS rules and Docker's), never
from user text. `create` is get-or-create per object, so a retry after a crash completes what is missing.
Target names are unique already (`409`), so a client retrying `POST /targets` can't make two machines.

**Reconcile on restart and on every probe.** At startup once each plugin is up, and then on the target probe
interval (`LOOMUX_TARGET_PROBE_INTERVAL`, 5 min), the Manager compares the `environments` table with
`targets.list`:

| Registry says | Plugin says | Action |
|---|---|---|
| `creating` | partial or nothing | resume `create` (idempotent) |
| `running` | running | nothing; the target probe decides readiness |
| `running` | gone (evicted node, deleted by hand) | persistent: `start` again (same address, same data), log it; ephemeral: status `lost`, its workspaces `archived` with a `status_reason`, the user told on the Machines page |
| `stopped` | running | `stop` (the registry is the intent) |
| `destroying` | anything | finish `destroy`, then delete the rows |
| `detached` | anything | nothing: the plugin that made it is gone |
| no row | objects with **this instance's** label | an orphan: logged, and destroyed once older than 24 h (the same TTL as the session orphan sweep) |

Objects without this instance's label are never touched, so a test and a production loomuxd sharing a cluster
can't reap each other's machines. The instance id is generated once and stored in the database (`settings`),
not configured: a restored backup keeps ownership of its machines, and a fresh database on the same cluster
can't claim them. Separate namespaces per instance (§10) are recommended on top.

## 2. From a created machine to a target

### 2.1 Transport: sshd in the machine vs `kubectl exec`

| | sshd in the machine, reached through the existing managed-mode `RemoteExecutor` | a new `TargetExecutor` over the Kubernetes exec API (SPDY/WebSocket) and Docker's exec API, proxied through the plugin |
|---|---|---|
| Code | none new on the executor side; the plugin only injects a key and a host key | a third executor, with its own versions of the LOOM-84/85 work (deadlines, slots, failure classes, `RunOnce` stdin scripts, paste-buffer round trips), and a streaming protocol between host and plugin |
| RBAC | `pods/exec` **not** needed: the most powerful namespaced verb stays ungranted | `pods/exec` on every agent pod: whoever holds the token can run anything in them |
| Load and latency | one multiplexed SSH connection per target, as today | every `send-keys`/`capture-pane` is an API-server exec session, audited and throttled by the API server, relayed through the plugin |
| Two plugins | identical path for Kubernetes and Docker, and for any later VM or VPS plugin (they all have sshd) | two exec APIs to implement and keep in step; a VM plugin would need ssh anyway |
| Human attach | `kubectl exec -it … tmux attach`, `docker exec`, or ssh via a port-forward; attach-info names them | the same |
| Cost | sshd (OpenSSH) in the agent image, ~10 MB; a port the NetworkPolicy must allow from loomuxd only | nothing in the image; a streaming protocol in the plugin system |

**Recommendation: sshd in the machine.** It makes a created machine a normal managed target, which is the
whole point: the executor hardening, health, the sweeper and `test` all apply unchanged; every plugin, VM and
VPS ones included, shares one connection story; and the plugin protocol stays request/response. The exec API
is kept out of v1 entirely, so the ServiceAccount never needs `pods/exec`. The plugin model doesn't change
this: a plugin that can't offer sshd (none planned) would need a new executor capability, not a new transport
for these two.

### 2.2 Bootstrap: the key goes in, the host key comes from the host

Two things make the first connection work without a scan, a TOFU step or a person:

- **authorized_keys**: the target's managed public key (generated as for any target, origin `target`, one key
  per machine) is passed in `spec.ssh.authorized_key`, prefixed with
  `no-port-forwarding,no-agent-forwarding,no-X11-forwarding` (not `no-pty`: tmux needs one). No `from=`
  restriction: loomuxd's pod IP isn't stable, and the NetworkPolicy (§4) is what limits who reaches port 2222.
- **The host key is generated by the host**, not by sshd on first boot: ed25519, private half passed in
  `spec.ssh.host_private_key` for the plugin to place in the machine, public half **pinned on the target row
  before the machine exists**. So managed mode's "no pin, no connection" rule is satisfied from the start,
  there is no window in which a different machine could answer, and `start`/`recreate` reuse the same host key
  so the pin never changes. `scan-host-key`/`pin` answer `409` for a created machine, with the reason.

How the two files reach the machine is plugin-specific (§8, §9); in both the image's entrypoint copies them
into a private 0700 directory with 0600 modes before starting sshd, the same pattern as `deploy/entrypoint.sh`
uses for `~/.ssh` (a Secret volume is root-owned 0644, and sshd's `StrictModes` wants the key private).

```jsonc
// targets.create params (the host builds it; the plugin never sees anything else)
{"id": "k7f3q2", "target_id": "…", "name": "builds", "size": "medium", "persistent": true, "egress": "internet",
 "image": "ghcr.io/loomux/agent@sha256:…",
 "ssh": {"authorized_key": "no-port-forwarding,… ssh-ed25519 AAAA… loomux-builds", "host_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----…",
         "host_public_key": "ssh-ed25519 AAAA…", "port": 2222},
 "labels": {"loomux.io/instance": "…", "loomux.io/target": "…", "loomux.io/environment": "k7f3q2"}}
```

### 2.3 Addressing

- **Kubernetes**: the pod sets `hostname: lx-<id>` and `subdomain: <subdomain>`; one headless Service named
  `<subdomain>` (created once by the manifests, selector `loomux.io/role=agent`) makes
  `lx-<id>.<subdomain>.<namespace>.svc.cluster.local` resolve to the pod's current IP. The target's `host` is
  that name (it passes the managed-mode host grammar), `ssh_port` 2222, `ssh_proxy: none` (in-cluster; the
  Tailscale sidecar isn't in the path). A restarted pod gets a new IP and the same name, and the same host key,
  so nothing on the target row changes. The host knows the name before `create` because the plugin's
  `targets.describe` reports its `address_template`.
- **Docker**: the container publishes 2222 on the docker host's configured `bind_address` (its tailnet IP) at a
  host port the plugin probes once from Docker's ephemeral range and then fixes for the machine's life (§9),
  returned by `create`; the target's `host` is the docker host's address from the plugin's configuration,
  `ssh_port` that port, `ssh_proxy` what the plugin's configuration says (`default` through the sidecar from
  the cluster). When loomuxd runs as a bare binary on the docker host itself, `bind_address` may be
  `127.0.0.1`.

### 2.4 Workspaces: one machine per target, one volume per machine

**A machine is a target, not a workspace.** The registry's model (a workspace is a directory on a target;
the router picks a target when it provisions) stays as it is, and a machine made for "project X" simply gets
its first workspace provisioned there the usual way. A per-workspace machine (pod per workspace, with the
target 1:1 to it) is a later mode if wanted, not v1: it would need the router to create machines and a
different cost model (one PVC per repository).

Each machine has one volume mounted at `/data`:

| Path | Role | Why one volume |
|---|---|---|
| `/data/work` | the target's `workspace_root`; workspaces are directories under it, confined as today | one PVC per machine keeps Ceph's object count and the quota simple |
| `/data/home` | `$HOME` of the agent user: `~/.cache/loomux/completion-markers`, `~/.claude` (`CLAUDE_CONFIG_DIR=/data/home/.claude` so `.claude.json`, which holds the account and the folder trust, lands in the volume too), `~/.codex`, git config | a one-time interactive sign-in and the folder trust survive restarts and image updates |

`persistent: true` (default) backs `/data` with a PVC (Kubernetes) or a named volume (Docker), sized by the
chosen size's `disk`; `stop` frees the pod/container and keeps it; `recreate` (a new image, another size)
keeps it; `destroy` deletes it. `persistent: false` backs `/data` with an `emptyDir` / anonymous volume: cheap
scratch for throwaway work, gone with the machine; `stop` isn't offered for it (it would be a destroy), and a
lost ephemeral machine archives its workspaces with the reason. In both cases `/` is read-only, `/tmp` is a
tmpfs (tmux's socket and the paste buffers live there), and the health probe's disk-free number is `/data`'s.

The existing workspace rules hold unchanged: `DELETE /workspaces/{id}` never touches files; a `failed`
workspace is kept for inspection; the 1 GiB free rule applies to `/data`.

## 3. The agent image (`ghcr.io/loomux/agent`)

Built from this repo (`deploy/agent/Dockerfile`, `deploy/agent/entrypoint.sh`, `deploy/agent/sshd_config`) by
the plugins workflow (`plugins.yml`, §11) on the same version line as the server: tag `<version>` for a
release, `main` for a tested main build, plus the digest. A plugin's configuration names the image
(`agent_image`, defaulting to the one the plugin was built with), exactly as the manifests pin the server
image.

**Contents.** Debian slim (glibc: the agent CLIs ship native binaries), Node.js 22 LTS (Claude Code requires 22
or later; Codex is an npm package too), `@anthropic-ai/claude-code@<pinned>`, `@openai/codex@<pinned>`,
`opencode-ai@<pinned>` (each pinned to an exact version, above the floors in `agents/README.md`, with
`DISABLE_AUTOUPDATER=1` so a container never drifts from what was tested), `tmux`, `openssh-server`, `git`,
`python3` (the Claude Code folder-trust script), `ripgrep`, `curl`, `ca-certificates`, `tzdata`, `jq`, and a
minimal build toolchain only if a size budget allows it (open question 10). No Docker CLI, no kubectl, no
cloud CLIs: an agent that needs one asks for it in chat and the install offer runs in its pane like on any
target.

**User.** `agent`, uid/gid 10002, `$HOME=/data/home`. Distinct from the server's 10001 so a misconfigured
volume can never be read across. The image also sets `CLAUDE_CONFIG_DIR=/data/home/.claude`.

**sshd.** Runs as `agent` (not root: no privilege separation user, no setuid), `Port 2222`,
`HostKey /run/loomux/ssh/host_ed25519` (copied in by the entrypoint from wherever the plugin put it, 0600),
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
token, the docker socket, the plugin's own credentials. The pod's environment is only what the image sets;
the pane's environment is the image's plus the vault values for that workspace, agent and target.

**Updates.** The environment records the image reference and digest it runs. A machine whose digest differs
from its plugin's configured image shows `update_available: true`, and `POST /targets/{id}/recreate` replaces
it with the configured image, keeping `/data` and the address. It is refused (`409`) while a task is mid-turn
or taken over. There is no in-place `npm update`: a version change is an image change, reviewed as a server
PR bumping the pin, like a web bump.

## 4. Security and threat model

The single-user assumption of v1 holds: everything a plugin makes belongs to the one user, and the routes
below join the "admin-only once there are roles" list the freeze review keeps (section D).

| Asset / threat | Mitigation |
|---|---|
| **Infrastructure credentials** (the ServiceAccount token = make/delete pods in one namespace; the docker host's SSH = root-equivalent on that host, since the docker group is) | they belong to the plugin, not the host: stored encrypted in the plugin's config and handed over on the private channel (subprocess form), or **mounted into the plugin's container only** (sidecar form, the recommended one on Kubernetes), where loomuxd's container has no token at all. Kubernetes: a dedicated ServiceAccount for the plugin bound to a **Role in the agents namespace only** (pods, PVCs, Secrets: create/get/list/watch/delete; `pods/log` get; Events list; **no `pods/exec`, no `services`, nothing cluster-wide, nothing in the `loomux` namespace**). Docker: the plugin's own SSH key to the docker host, pinned host key, no docker TLS client certs |
| **A compromised loomuxd** (its HTTP surface, a token) | can only do what the protocol offers: make and destroy labelled machines within the plugin's quota, never arbitrary API calls, in the sidecar form. In the subprocess form the token is in the same container: no boundary, stated in §1.6 |
| **A malicious or buggy plugin** | runs out of process; its output is validated (addresses by grammar, statuses by list, strings bounded); it never receives the vault, the managed private keys or another machine's material; a third-party one must be explicitly trusted with its declared permissions. It **is** trusted with everything on the machines it makes (§1.6): first-party plugins are therefore the bundled ones |
| **The docker socket inside an agent container** | never mounted; the plugin talks to the engine from its own process, and the agent image has no docker client. Agent containers get `--cap-drop ALL`, `--security-opt no-new-privileges`, a non-root user, `--read-only`, `--pids-limit` |
| **An agent reaching loomuxd** (its API, its database PVC, its SOCKS sidecar) or the cluster | agents live in **another namespace** (`loomux-agents`) with a default-deny ingress NetworkPolicy (only loomuxd's pod may reach 2222) and an egress policy that allows kube-dns and the internet but **denies the cluster CIDRs, RFC 1918, the Tailscale CGNAT range, link-local and loopback** (`ipBlock` with `except`), so a pod can't reach the API server, loomuxd, Ceph, the LAN or the tailnet. Egress `none` cuts the internet too. **This depends on a policy engine** (next row) |
| **NetworkPolicy not enforced** (theWyseKube today: Talos-default Flannel v0.27.4 with no policy controller; a deny-all policy was proven to block nothing) | the Kubernetes plugin's `check` **tests enforcement** (§8): it runs a short canary pod labelled `egress=none` that tries to reach the API server's ClusterIP and an external address and exits with the result, read from the pod's status (no exec). Not enforced ⇒ a warning on the plugin and on every machine it makes ("Network isolation is not enforced on this cluster: agents can reach cluster services, the LAN and the tailnet"), the `egress` choice is withdrawn from `targets.describe` (so the UI can't offer `none` and the user can't believe a machine is confined), and the plugin's setting `require_network_policy` (default `false`) decides whether machines may be created at all; **set it to `true` for production** (§10). An engine (theWyseKube's first choice is kube-network-policies, a policy-only nftables DaemonSet) makes the warning go away on the next `check` |
| **An agent reaching another agent's machine** | default-deny ingress between pods in the namespace; `enable_icc=false` on Docker |
| **An agent escaping the container** | Pod Security Admission `restricted` on the namespace: `runAsNonRoot`, seccomp `RuntimeDefault`, all capabilities dropped, no privilege escalation, read-only root, no host namespaces or host paths; the plugin's pod spec is written to pass `restricted` and CI asserts it. A sandboxed runtime (gVisor/Kata `RuntimeClass`) isn't in theWyseKube today and is left as a later plugin setting (open question 14) |
| **Resource exhaustion** (a runaway build, a fork bomb, a full disk) | requests and limits from the chosen size, `pids` limit, PVC size; a `ResourceQuota` and `LimitRange` on the namespace cap the total; `max_environments` per plugin instance caps the count; the 1 GiB free rule keeps provisioning off a full volume |
| **Secret exposure** | the managed private key stays in the host (in-process agent, LOOM-138); the machine's host private key is stored encrypted in the registry and placed in the machine via a Secret only the plugin's Role can read, mounted into only that pod; the `authorized_keys` line is public material. Plugin secrets are write-only through the API, and **text a plugin produces is scrubbed of the plugin's own configured secret values** before it is stored, shown or logged: its stderr lines, RPC error messages and check problems reach `status_reason`, `instance_reason`, `check.problems` and the log only after that pass (the LOOM-156/157 redaction, applied with the plugin's secrets). Vault values reach the pane's environment at launch, as on every target, and are visible to that agent by design, never written to the volume by Loomux. Pod names, labels and annotations carry ids and the target name, never hosts, users or key material |
| **A compromised agent image** (supply chain of three npm packages) | exact version pins, `DISABLE_AUTOUPDATER`, the image rebuilt only by a reviewed PR, pinned by digest in the plugin's configuration, scanned in CI (`trivy`, fail on critical) |
| **A compromised plugin artifact** | first-party plugins verified by attestation at install when not bundled; bundled ones share the image's supply chain; third-party ones need explicit trust |
| **Name/argument injection** (user text into object names, ssh argv, the protocol) | object names derive from random ids only; the target's host is the deterministic DNS name and passes the managed-mode grammar; the user's name is a label value, validated against Kubernetes' label grammar or dropped to an annotation; plugin config values pass the schema before they reach the plugin |
| **Who may install plugins, create, stop, destroy machines** | every route is behind `requireAuth`; with roles these are admin routes |
| **Lost master key** | plugin configs and host keys become unreadable: plugins show `error` and must be reconfigured; created machines fail like any managed target and are recreated. Documented in backup-restore |
| **A second Loomux instance on the same cluster** | instance id label on every object and `targets.list` filtered by it (the plugin receives the id at `configure`); separate namespaces recommended |

**Multi-tenant assumptions.** One user, one namespace, one quota. Nothing in this design assumes otherwise,
and nothing prevents a later per-user namespace: the Manager keys everything by instance id, a user id would
be one more label and one more plugin configuration parameter.

## 5. API additions (additive to v1)

All additions; nothing existing changes shape or meaning. Each is recorded in the contract golden file with
`-update` in its PR.

**Plugins** (§1.5)

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/plugins/available` | the catalog: `{plugins: [{name, title, version, protocol, vendor, source, path, trust, isolation, capabilities, permissions, config_schema, installed: [ids]}]}` |
| `GET /api/v1/plugins` | installed instances: `{plugins: [{id, name, label, version, available_version, protocol, source, trust, status, status_reason, enabled, capabilities, config (secrets as {set: bool}), check: {ok, problems: [{code, message, severity}]}, machines: n, installed_at, updated_at}]}` |
| `POST /api/v1/plugins` `{plugin, source?, label, config}` | install; `201` with the instance; `400` (schema), `409` (label), `503` (no master key), `422` with the `check` problems when it starts but can't work |
| `GET /api/v1/plugins/{id}` | one instance, as in the list |
| `PUT /api/v1/plugins/{id}/config` | reconfigure; secrets omitted keep their value, `""` clears |
| `POST /api/v1/plugins/{id}/enable`, `.../disable`, `.../upgrade`, `.../check` | lifecycle; `check` re-runs the plugin's self-test and returns its problems |
| `DELETE /api/v1/plugins/{id}?targets=destroy\|keep` | uninstall; `409` with the targets when it has any and the parameter is absent |

**Targets**

- `POST /api/v1/targets` accepts a new optional object `plugin: {id, size, persistent, egress}`. With it,
  `kind` must be `remote` or omitted, and `host`, `user`, `ssh_port`, `ssh_key_id`, `generate_ssh_key`,
  `ssh_proxy`, `workspace_root` must be absent (`400` naming the field): the plugin sets them. The policy
  fields work as before. Answer: `201` with the target, `plugin.status: creating`; creation continues in the
  background. `422` when the plugin's `check` isn't ok (the problems in the body), including
  `require_network_policy` refusing.
- Target responses gain `plugin` (null for a registered host):
  `{id, name, label, version, environment_id, status, status_reason, size, persistent, egress, image,
  image_digest, update_available, warnings: [{code, message}], created_at}`. `warnings` carries the plugin's
  machine-level warnings (network isolation not enforced). `ready`/`next_step` keep their meaning; `next_step`
  stays null while the machine is coming up, and `plugin.status` says why the target isn't ready.
- `GET /api/v1/targets/{id}` (new; listed as addable in the freeze review), so the web can poll one target
  while it is created.
- `PUT /api/v1/targets/{id}` on a created machine refuses (`400`) the connection fields it refuses on create;
  name, `permission_mode`, policy and `relay` stay editable.
- `POST /api/v1/targets/{id}/stop`, `POST /api/v1/targets/{id}/start`,
  `POST /api/v1/targets/{id}/recreate` (body `{size?}`; the image is always the plugin's configured one):
  `202` with the target; `409` while a task there is mid-turn or taken over, `409` for `stop` of an ephemeral
  machine or when the plugin lacks the capability, `409` when the plugin is disabled or detached,
  `404`/`409` for a registered host.
- `DELETE /api/v1/targets/{id}` on a created machine **cascades**: refused (`409`, listing what blocks) while
  any task is mid-turn or taken over; otherwise it deletes the target's workspaces with their tasks (as
  `DELETE /workspaces/{id}` does, one transaction), its target-scoped credentials, destroys the machine (data
  included) through the plugin, deletes the generated key and the rows. A `detached` machine (its plugin
  uninstalled with `targets=keep`) is deleted the same way minus the destroy, with the response saying the
  machine itself must be removed by hand. For a registered host the existing rule (refuse while workspaces
  exist) is unchanged.
- `scan-host-key`, `pin`, `DELETE .../pin`, `migrate-ssh` answer `409` on a created machine.

**Credentials**: `POST /api/v1/credentials` accepts `target_id`; `GET` lists it; the uniqueness rule becomes one
name per (workspace, target, agent_type) scope.

**Attach-info**: gains `attach_commands: [{via, command}]` beside the unchanged `attach_command`, from the
plugin's `targets.attach_commands`: for a Kubernetes machine `via: "kubectl"`, `kubectl -n <ns> exec -it
<pod> -- tmux -L loomux attach -t <session>` (the pod's DNS name isn't resolvable from a laptop; this is);
for Docker `via: "docker"`, `ssh <host> docker exec -it <container> tmux -L loomux attach -t <session>`;
`via: "ssh"` with the plain form for every target. The host validates the returned commands against a
grammar (no newlines or control characters, bounded) and shows them as text; it never runs them.

**Health and metrics**: `GET /health/deep` gains a `plugins` component (each instance's `check`);
`loomux_plugins{plugin,status}`, `loomux_environments{plugin,status}` and
`loomux_plugin_calls_total{plugin,method,outcome}` are exported. Lifecycle steps are logged as structured
records (`plugin start`, `environment create`, with ids and the plugin's message, never key material or
config secrets).

## 6. Web UX (loomux/web, separate PR)

**Plugins page** (Settings → Plugins): a catalog of available plugins as cards (title, vendor, version,
source, a trust line: "Part of this Loomux release" / "Signed by Loomux CI" / "Not signed by Loomux"), each
with **Install**. The install dialog shows the manifest's permissions as a list the user must scroll
through, the capabilities in plain words ("creates machines; can stop and start them; can keep their data"),
a form generated from `config_schema` (secrets as password fields with "set" state, `ssh-private-key` fields
with a **Generate** button that shows the public half to copy into `authorized_keys`), a **label** field, and
for a third-party plugin the explicit trust checkbox naming the isolation it has. Installing runs the check
and shows its problems inline (with the `network_policy_not_enforced` warning rendered as a yellow banner,
not an error). Installed instances show status, `check` problems, **Configure**, **Disable**/**Enable**,
**Upgrade** (when `available_version` differs, with both versions), **Uninstall** (a dialog listing its
machines with the two choices: destroy them, or keep them as plain hosts).

**Machines page** (the Targets page, renamed in the web; the API keeps `targets`): registered hosts and
created machines in one list, with filter chips **All / Registered / Created**. **Add machine** opens a
two-way choice:

- **Register a host** — the LOOM-138 wizard (host, user, verify host key, authorize, test), unchanged.
- **Create a machine** — shown only when an installed, enabled plugin passes its check. Fields: plugin
  instance (hidden when there is one), name, size (cards with cpu/memory/disk), **Keep data** toggle
  (persistent, on by default, shown only with `targets.persistent`+`targets.ephemeral`), egress (Internet /
  None, shown only when the plugin offers it; absent with a one-line reason when network isolation isn't
  enforced), and the usual policy block collapsed. **Create** posts and the new card appears at once with a
  progress strip driven by `GET /targets/{id}`: Creating → Starting → Ready, or an error with the plugin's
  reason and a **Retry** (recreate) button.

A created machine's card carries a badge **Created by Kubernetes · wysekube** (`plugin.label`), `medium ·
keeps data`, the usual health line, its warnings, and actions **Stop**/**Start** (with `targets.stop_start`),
**Update** (only when `update_available`, with the configured image named), **Delete** (a confirmation
listing the workspaces and saying the data goes with it; for a detached machine, that the machine itself must
be removed by hand). The attach panel shows the `kubectl`/`docker` command from attach-info first, the ssh
form second. A stopped machine's workspaces are shown greyed with "machine stopped"; a dispatch to one starts
it first (§7). A machine whose plugin is disabled or removed says so on the card and loses the lifecycle
buttons.

## 7. The router's view of plugins

The routing model sees a created machine as it sees any target: id, name, kind `remote`, agents, problem,
policy (`TargetSnapshot`), plus two new fields: `CreatedBy` (the plugin name, or empty) and `Ephemeral`.
The prompt says what they mean: "a machine Loomux created; only its own workspaces are there; an ephemeral
one loses its files when it is destroyed". Policy defaults for created machines are the permissive ones
(purpose personal, provision and shell allowed, no confirmation), since nothing else runs there, and the user
can tighten them in the form. The model never sees plugin names beyond that, nor configuration.

**Dispatch to a stopped machine.** A `use_workspace`/`provision_workspace`/`run_command` aimed at a target
whose machine is `stopped` starts it first (a dispatch stage, bounded like a provisioning run, shown in the
audit trail as an event) and then goes on; `creating`/`starting` waits for readiness up to the same bound;
`error`/`lost`/`detached`-and-unreachable fails the dispatch with `target_unhealthy` and the reason. The
snapshot's `Problem` says "stopped; will be started" so the model doesn't avoid the target.

**No machine creation by the router in v1.** `provision_target` as a routing action (the model asking for a
new machine, behind a require-confirmation offer with the plugin, size and what it costs shown, like a clone
it wasn't told about) is a natural follow-up once a plugin has been used by hand for a while. Until then
machines are made from the Machines page or `POST /targets`, and a request that needs one gets the LOOM-68
style direct answer naming the page.

## 8. The Kubernetes plugin (LOOM-179)

**Packaging.** Its own Go module, `plugins/kubernetes` (so its dependencies never enter the server's
`go.mod`), with `plugins/kubernetes/cmd/loomux-plugin-kubernetes` inside the module (a `cmd/` at the root
would pull client-go into the server's module). The module replaces `github.com/Loomux/server` with `../..`;
no `go.work` is committed, so the root's `go build ./...`, `go vet` and `govulncheck` stay in module mode and
the server image's build is unchanged (`go work init . ./plugins/kubernetes` is a local convenience). Built three ways by `plugins.yml`: a binary bundled in the
server image at `/usr/local/lib/loomux/plugins/kubernetes/` (with `plugin.json`), a signed release asset,
and the image `ghcr.io/loomux/plugin-kubernetes` (distroless static: a pure-Go plugin execs nothing). Started
with no arguments it serves stdio; with `--listen /run/loomux/plugins/kubernetes.sock` it serves the socket
(the sidecar form). Its stderr is its log.

**Client.** `k8s.io/client-go`, in-cluster (the mounted ServiceAccount: sidecar form) or from the
`kubeconfig` secret in its configuration (a subprocess outside the cluster, tests). Revision 1 preferred a
hand-written REST client to keep the server's dependency graph small; with the plugin as its own module that
reason is gone, and client-go's in-cluster and kubeconfig handling, typed objects and watch are worth having
(open question 18).

**Configuration** is the schema in §1.4 plus `require_network_policy` (boolean, default `false`; see
below) and, later, `runtime_class`. `targets.describe` reports the sizes, the image, the address template
`lx-{id}.<subdomain>.<namespace>.svc.cluster.local:2222`, `ssh_proxy: none`, and the egress options, which it
withdraws when network isolation isn't enforced.

**Objects per machine**, all labelled `app.kubernetes.io/managed-by=loomux-plugin-kubernetes`,
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
    env: [{name: HOME, value: /data/home}, {name: CLAUDE_CONFIG_DIR, value: /data/home/.claude}, {name: TZ, value: <host's>}]
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
copy, and the plugin learns the pod is up from `status.phase`/`containerStatuses`, the host from the target
probe over SSH.

**Lifecycle mapping.** `create` = Secret, PVC, Pod (get-or-create each). `stop` = delete the Pod (Secret and
PVC stay). `start` = create the Pod again. `recreate` = delete the Pod, create it with the new image/size.
`destroy` = delete Pod, PVC, Secret (each tolerating 404). `health` = phase, container state, restart count,
the last Event message on failure (`ImagePullBackOff`, `Pending` on a PVC that can't bind, an evicted pod),
reported in `status_reason` in the plugin's words. Bare Pods rather than a StatefulSet/Deployment: the host
reconciles on every probe already, there are no `apps` RBAC verbs to grant, and a one-replica StatefulSet
would still need the same volume and address handling (open question 9).

**The network-isolation check.** NetworkPolicy is only as real as the cluster's policy engine, and
theWyseKube has none today (Talos-default Flannel, no policy controller: a deny-all policy was proven to
block nothing). So `plugin.check` **tests enforcement rather than assuming it**: it creates a short-lived
canary pod `lx-netcheck-<rand>` from the agent image, labelled `loomux.io/role=agent` and
`loomux.io/egress=none` (which the §10 policies confine to kube-dns only), restricted-compliant like every
other pod, running
`timeout 3 bash -c 'exec 3<>/dev/tcp/<API server ClusterIP>/443' && exit 10; timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && exit 11; exit 0`,
then reads the exit code from the pod's status and deletes it. Exit 0 means both were blocked (enforced);
10 or 11 means a `none` pod could reach the cluster or the internet (not enforced). It runs at install, after
a configuration change, on `POST /plugins/{id}/check`, and once every six hours. Not enforced:

- `check` returns the problem `network_policy_not_enforced` with severity `warning`, shown on the plugin
  and as a warning on every machine it makes;
- `targets.describe` withdraws `egress_options`, so the UI offers no choice and no machine claims to be
  confined; machines are made as `internet` with the warning attached;
- with `require_network_policy: true` the problem's severity is `error`: `check` fails and no machine can be
  created until an engine is installed. **Production should set it to `true`; TEST may run `false`** while
  the user decides on an engine (§10), with the warning in view.

**Image pull.** The agent image is public on GHCR, like the server's; no `imagePullSecrets`. A private image
would be one more Secret the Role can read and `imagePullSecrets` on the pod.

**Ready signal.** `running` once the pod is `Running` with the container ready (tcp 2222 answering); the
host's target probe (`ssh` + `tmux -V`) then sets `ready`. Both are needed: a pod can be `Running` with sshd
refusing the key (a copy error), which the `test` steps show as `auth` failed, with the pod log named in the
hint.

**RBAC it needs** (the plugin's ServiceAccount, in the agents namespace only): `pods` (create, get, list,
watch, delete), `pods/log` (get), `persistentvolumeclaims` (create, get, list, delete), `secrets` (create,
get, list, update, delete; update carries a recreate's new spec), `events` (list), `services` (get: `check`
warns when the headless Service is missing, or when it can't tell). theWyseKube's manifests (its PR #25)
grant exactly this, and so does the kind Role CI runs the plugin under (`deploy/test/kind/`), so a verb it
needs and the Role lacks fails the integration test.

## 9. The Docker plugin (LOOM-180)

**Packaging.** Module `plugins/docker`, `cmd/loomux-plugin-docker`, bundled, released and imaged like the
Kubernetes plugin. Pure Go: `golang.org/x/crypto/ssh` for the connection to the docker host,
`golang.org/x/net/proxy` for a SOCKS5 hop, the Engine API over plain `net/http` (no Docker client library:
the handful of endpoints used are small and the plugin controls every request).

**Configuration** (`config_schema`):

| Field | Meaning |
|---|---|
| `engine` | `ssh://user@host[:port]` (the docker host; the user must be in the docker group, which is root on that host and is said so in the field's description) or `unix:///var/run/docker.sock` for a plugin running on the docker host itself |
| `ssh_private_key` (`x-secret`, `x-format: ssh-private-key`) | the plugin's **own** key to the docker host, generated by the host's **Generate** button or pasted; its public half is shown to append to the host's `authorized_keys`. The plugin never uses loomuxd's managed keys |
| `ssh_host_key` | the docker host's pinned host key line. Empty at first: `check` then returns the problem `host_key_unpinned` with the scanned type and fingerprint, and the UI's **Trust this key** writes it to the config, the LOOM-114 pattern reused |
| `proxy` (`x-format: socks5-url`, optional) | `socks5://127.0.0.1:1055` when the docker host is only on the tailnet and the plugin runs in the pod |
| `bind_address` | the docker host's address the agent containers publish 2222 on (its tailnet IP; `127.0.0.1` for a local engine) and what created machines' `host` becomes |
| `ssh_proxy` | `default` or `none`: how the **host** reaches the created machines (through `LOOMUX_SSH_PROXY` or directly), reported by `targets.describe` |
| `agent_image`, `sizes`, `max_environments` | as for Kubernetes |

**Transport.** The plugin dials SSH (pinned host key, its own key, through the proxy if set), runs `docker
system dial-stdio` on the host, and speaks the Engine API (HTTP) over that session's stdio, the same thing the
Docker CLI's `ssh://` contexts do. One multiplexed SSH connection per plugin instance, reconnected on loss.

**Objects per machine**, labelled like the pods (`loomux.io/*` labels on Docker objects):

| Object | Name | Notes |
|---|---|---|
| Network | `loomux-agents` (shared, created once per host) | user-defined bridge, `com.docker.network.bridge.enable_icc=false` (no container-to-container). No `--internal` network: Docker publishes no port on one, so an egress-`none` machine's sshd would be unreachable (found at implementation); **`egress: none` is withdrawn** and the plugin declares no `targets.egress_policy` |
| Volume | `lx-<id>-ssh` | the record: sshd's two files, the spec and the port as labels (see Bootstrap) |
| Volume | `lx-<id>-data` | only for `persistent: true`; an anonymous volume otherwise |
| Container | `lx-<id>` | below |
| Container | `lx-<id>-init` | transient: probes the port, writes the record's files; removed in the same call |

**The container**: image `<agent_image>`, `--user 10002:10002`, `--read-only`, `--tmpfs
/tmp:size=1g,mode=1777`, `--tmpfs /run/loomux:size=1m,mode=1777` (Docker mounts a tmpfs root-owned 0755
otherwise, and the entrypoint runs as the agent), `--cap-drop ALL`, `--security-opt no-new-privileges:true`,
the daemon's default seccomp profile (`seccomp=default` isn't a value the API takes; `check` warns when the
daemon runs without seccomp), `--pids-limit 512`, `--init` (sshd is process 1 and reaps nothing: orphans
would pile up as zombies against the pids limit), `--memory`/`--cpus` from the size, `--mount
type=volume,source=lx-<id>-ssh,target=/etc/loomux/ssh-src,readonly`, `--mount
type=volume,source=lx-<id>-data,target=/data`, `--network loomux-agents`, `--publish
<bind_address>:<port>:2222` with **the port fixed** (a `HostPort` of 0 is allocated anew at every start, found at
implementation; so `create` probes one once with the helper published on `<bind_address>:0:2222`, reads it
back from `inspect`, removes the helper and bakes the port into the record and the container, and a port
found taken at start rebuilds the record with a fresh probe), a health check `bash -c 'exec
3<>/dev/tcp/127.0.0.1/2222'` every 5 s standing in for the readiness probe (`running` means healthy),
`--restart unless-stopped`, env `HOME`, `CLAUDE_CONFIG_DIR`, `TZ` as for the pod, labels as above. Nothing
from the host is mounted; `/var/run/docker.sock` is never passed. The image isn't pulled by the engine: a
missing one is pulled by the plugin in the background, `create` (and `recreate` onto a new image, and `start`
remaking a lost container) answering `creating`/`recreating`/`starting` with the phase ("pulling the image")
after 20 s and finishing when the pull ends; a destroy or a recreate cancels a making in progress and waits
for it.

**Bootstrap without Secrets.** Docker (outside Swarm) has no Secret object, and `PUT /containers/{id}/archive`
is refused on a `--read-only` container ("container rootfs is marked read-only"; found at implementation,
Docker 29.8). So the files go into a named volume, **`lx-<id>-ssh`, the machine's record** (the Secret's
analogue): the plugin creates it with labels carrying the spec without key material, the creation time and the
fixed ssh port, then uploads a tar with `host_ed25519` and `authorized_keys` (0400, uid 10002) into it through
a transient helper container `lx-<id>-init` that mounts the volume read-write and is never started (the
daemon does extract into a created container's volume). The machine's container mounts the volume read-only
at `/etc/loomux/ssh-src`; the entrypoint copies the files to `/run/loomux/ssh` (tmpfs) exactly as in the pod.
While the record exists the machine exists: `stopped` when its container is gone and its data volume stays,
`lost` otherwise. A `recreate` remakes the record and uploads again; `start` of a persistent machine whose
container is gone makes the container again from the record.

**Lifecycle mapping.** `create` = image (pulled if missing), network (get-or-create), the port probed, the
record volume with its files, the data volume, the container, start. `stop` = `stop` (the container and its
published port stay; the port is fixed on first create and recorded in the record, so the target's `ssh_port`
never changes). `start` = `start`, or the container again from the record when it is gone and the data volume
stays. `recreate` = `rm` the container and the record, make both again with the same name, port and data
volume. `destroy` = `rm -f` the container and any helper, remove the data volume, then the record (last, so a
crash in between leaves a record the host's reconcile destroys again). `health` = `inspect` state, health
check, exit code, restart count, and the container's last log line for an error. `list` = records by label.
Orphan cleanup = records with this instance's label and no row.

**What Docker can't give**, declared in the manifest's description and shown at install: no NetworkPolicy
(the LAN is reachable from a container; and since a port published on an `--internal` network isn't mapped
at all, there is no `egress: none` either: the plugin offers no egress choice), no quota beyond per-container
limits (so `max_environments` matters more), no PSA (the plugin asserts its own flags instead, and a test pins
them), and the docker host's own security is the user's.

## 10. What theWyseKube would need (handoff to command-center; nothing edited here)

For the TEST instance (`services/base/loomux`), mirrored later for `loomux-prod` with its own namespace. The
recommended form is the **sidecar**: loomuxd's container never holds a Kubernetes credential.

1. **Namespace** `loomux-agents`, labelled `pod-security.kubernetes.io/enforce: restricted` (and
   `warn`/`audit` the same), `app.kubernetes.io/part-of: loomux`.
2. **ServiceAccount** `loomux-plugin-kubernetes` in `loomux`, set as the loomuxd pod's `serviceAccountName`
   with `automountServiceAccountToken: false` on the pod; its token is a `projected` volume
   (`serviceAccountToken`, `expirationSeconds: 3600`) **mounted only into the plugin container**. A **Role**
   in `loomux-agents` with the verbs of §8 and a **RoleBinding** to it. No ClusterRole, no `pods/exec`,
   nothing in `loomux` itself.
3. **The plugin sidecar** in the loomuxd Deployment: container `plugin-kubernetes`, image
   `ghcr.io/loomux/plugin-kubernetes@sha256:…` (pinned like the server image), args
   `["--listen", "/run/loomux/plugins/kubernetes.sock"]`, `runAsUser: 10003`, `runAsGroup: 10001` (so the
   socket it creates at mode 0660 is connectable by loomuxd's uid 10001 through the pod's `fsGroup`),
   read-only root, all capabilities dropped, no privilege escalation, small requests/limits; an `emptyDir`
   `plugins-sock` mounted at `/run/loomux/plugins` in **both** containers; the token projection mounted in
   this container only. loomuxd gets `LOOMUX_PLUGIN_SOCKET_DIR=/run/loomux/plugins` (the image's default).
   The plugin's configuration (namespace, storage class, image, sizes, `require_network_policy`) is entered
   in the UI and stored in the database, not in the manifests (open question 19 offers a GitOps seed).
4. **Headless Service** `loomux-agents` in `loomux-agents`: `clusterIP: None`, selector
   `loomux.io/role: agent`, port 2222; `publishNotReadyAddresses: true` so the name resolves while sshd starts.
5. **A policy engine**, because NetworkPolicy is not enforced today (Flannel v0.27.4, no controller, proven
   with a deny-all test). theWyseKube's first choice, **kube-network-policies** (a policy-only nftables
   DaemonSet, the smallest change), is the one this design recommends; Calico policy-only on Flannel
   (Canal) or kube-router firewall-only would do the same; a Cilium migration is not worth it for this. Until
   one is installed, the plugin reports `network_policy_not_enforced`, offers no egress choice, and TEST runs
   with `require_network_policy: false` and the warning in view; **prod sets it `true`**.
6. **NetworkPolicies** in `loomux-agents`, which do nothing until item 5 and everything after it:
   (a) default deny ingress and egress for `loomux.io/role=agent`;
   (b) allow ingress to TCP 2222 from `namespaceSelector: {kubernetes.io/metadata.name: loomux}` +
   `podSelector: {app: loomuxd}`;
   (c) an explicit egress rule to kube-dns: `namespaceSelector: {kubernetes.io/metadata.name: kube-system}`
   + `podSelector: {k8s-app: kube-dns}`, UDP and TCP 53;
   (d) for `loomux.io/egress=internet` pods, allow egress to `0.0.0.0/0` **except** `10.0.0.0/8` (covers the
   pod CIDR `10.244.0.0/16` and the service CIDR `10.96.0.0/12`, listed anyway for readability),
   `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` (the Tailscale CGNAT range), `169.254.0.0/16`,
   `127.0.0.0/8`; `none` pods match only (a)–(c).
7. **ResourceQuota** `loomux-agents` (TEST, to tune): `requests.cpu: 4`, `limits.cpu: 8`,
   `requests.memory: 8Gi`, `limits.memory: 16Gi`, `pods: 8` (the canary included), `persistentvolumeclaims:
   6`, `ceph-rbd-sc.storageclass.storage.k8s.io/requests.storage: 100Gi`; a **LimitRange** with the `small`
   defaults so a pod never runs unbounded.
8. **PBS backup**: `lx-*-data` PVCs are the user's working copies of repositories, not system state; recommend
   **not** adding them to the nightly PBS set in v1 (git remotes are the backup), stated in the handoff so it
   is a decision, not an omission.
9. **Prod**: nothing until TEST has run the plugin for a while; then `loomux-prod-agents` with the same shape,
   `require_network_policy: true`, and the policy engine in place first.

## 11. Phased PR plan

| PR | Scope | Depends on |
|---|---|---|
| **1. Plugin framework** (host side, LOOM-178 implementation part 1) | `plugins/`: manifest parsing and validation, the catalog over the three sources, the JSON-RPC client with `Content-Length` framing and per-call deadlines, the subprocess supervisor and the socket connector, `plugins` + `settings` tables (migration `00028_plugins`) with encrypted config, the plugin lifecycle API of §5 (install, configure, enable/disable, upgrade, check, uninstall; `targets=destroy|keep` is accepted and validated from PR 1, and never blocks until PR 2 gives a plugin machines to own), trust (bundled / attestation-verified via the LOOM-118 verifier / unsigned), `plugintest` (a conformance suite any plugin binary must pass) and `loomux-plugin-fake` (a test plugin), deep-health component, metrics, docs (`docs/deploy/plugins.md`), `core-design.md` §1/§7 paragraph | LOOM-138 (merged) |
| **2. Target-provider capability** (part 2) | `providers/`: the Manager, `00029_environments`, the `targets.*` client, target API additions of §5, cascade delete, vault `target_id` scope, attach-info `attach_commands`, router snapshot fields and the start-if-stopped dispatch stage; the fake plugin grows an in-memory `targets.*` | PR 1 |
| **3. Agent image and the plugins workflow** | `deploy/agent/{Dockerfile,entrypoint.sh,sshd_config}`; `plugins.yml` builds the agent image, every plugin binary and image, signs them (GitHub attestations), publishes `ghcr.io/loomux/agent` and `ghcr.io/loomux/plugin-*`; the server `Dockerfile` gains a stage that builds the plugin modules and bundles their binaries; `docs/deploy/agent-image.md` | none (can run beside PR 1) |
| **4. Kubernetes plugin** (LOOM-179) | `plugins/kubernetes` module, client-go, objects, pod spec, the enforcement check, `--listen` form; a `kind`-based CI job; the §10 handoff | PR 1, 2, 3 |
| **5. Docker plugin** (LOOM-180) | `plugins/docker` module: ssh dial-stdio and socket transports, objects, archive upload, host-key scan-and-pin; CI against the runner's docker | PR 1, 2, 3 |
| **6. Web** (loomux/web) | Plugins page, the Machines page with its two Add flows, created-machine cards, warnings, attach commands, update badge | PR 1, 2 (fake plugin for dev) |
| **7. Deploy** | theWyseKube changes landed by command-center (sidecar, policy engine); TEST installs the plugin from the UI, creates its first machine, a dispatch round-trip; then the user decides about prod | PR 4, §10 |
| later | `provision_target` routing action; per-workspace machines; a sandboxed runtime class; registry fetch at install; VM/VPS plugins (Proxmox, libvirt, a cloud provider) against the same protocol | use |

PR 1 ships with no capability but `plugin.*` and changes nothing for a deployment that installs no plugin;
PR 2 is where the target API additions land. Each plugin module has its own `go.mod` with a `replace` to the
root (no `go.work` is committed: the root's tooling stays in module mode), and CI runs `go test` per module
(`plugins.yml`).

## 12. Test strategy

- **Plugin conformance (`plugins/plugintest`).** The analogue of `storetest`/`executortest`: given a way to
  launch a plugin (a binary, or an in-process fake), the suite drives `describe` (manifest well-formed, matches
  disk), `configure` with valid and invalid config, `check`, then the `targets.*` lifecycle: create twice
  with the same id (idempotent), get, list filtered by instance, stop/start (if declared), recreate, destroy
  twice, health, attach commands within the grammar; and protocol conformance: unknown method → error, a
  malformed frame, a frame over 1 MiB (refused), a slow call hitting its deadline. The fake plugin passes it
  in unit tests; the real plugins pass it in their integration jobs.
- **Host framework, in-process.** Catalog over temp dirs and a socket; supervisor: crash → restart with
  backoff → `error` after three; hang → deadline; `shutdown` → kill after 10 s; a clean environment (no
  `LOOMUX_*` reaches the child, asserted by the fake echoing its env); install/configure/disable/upgrade/
  uninstall state machine; secrets encrypted with AAD, never in any response (the LOOM-139 JSON scan on every
  plugin route), a manifest that differs from `describe` refused; attestation verification with the LOOM-118
  fixtures; `targets=keep` leaves working targets; `targets=destroy` cascades.
- **Manager, in-process** (against the fake plugin with scriptable failures: create fails at step 2, the
  machine vanishes, the image pull hangs): the state machine, the reconcile table, the orphan TTL, the
  resume-after-crash of `creating` rows, the start-if-stopped dispatch stage; API contract additions via
  `TestAPIv1Contract -update`; negative tests: plugin fields on a registered host, connection fields on a
  created machine, sizes not offered, `stop` of an ephemeral machine, `pin`/`migrate-ssh` on a created
  machine, a second instance's objects never listed, a plugin's address outside the host grammar refused.
- **Kubernetes plugin.** (a) Unit tests with client-go's fake clientset, asserting every security field of the
  pod spec, the labels, the canary's exit-code handling. (b) An **integration job in CI** (`k8s-integration`,
  separate from `test`, required for plugin PRs): `helm/kind-action` brings up kind, the job builds the agent
  image and `kind load`s it, applies the §10 manifests (a test copy under `deploy/test/kind/`), runs the
  conformance suite against the plugin binary with a kubeconfig, then a `//go:build k8sintegration` host test
  creates a machine end to end: environment `running`, port-forward 2222, connect through the real
  managed-mode `RemoteExecutor` (AgentPool, pinned host key with the forwarded `[127.0.0.1]:port` host), run
  `tmux -V` and the `test` steps, `stop`/`start`/`destroy`, assert nothing is left. plain kind enforces no NetworkPolicy (CI's canary reached the API server on 2026-10-09, and still did with
  kube-network-policies installed into kind), so the job asserts the enforcement check reports `not enforced`
  there; every exit code is pinned by unit tests, and the `enforced` verdict is observed on TEST, where the
  engine runs (the test takes `LOOMUX_KIND_ENFORCED=1` for a cluster that enforces).
- **Docker plugin.** Unit tests against an in-memory Engine API (with Docker's port reallocation, archive
  refusal, anonymous volumes and label filters); an integration test on the GitHub runner's own docker over
  the unix socket (the conformance suite; then create, ssh in at the published port on `127.0.0.1` with the
  host key pinned, tmux, stop and start at the same port, recreate at the same port, destroy leaving nothing),
  and the same through SSH to the runner itself (`targets/sshtest`, whose shell runs the real `docker system
  dial-stdio`). The ssh dial-stdio transport's unit tests use `sshtest`'s `Exec` hook piping to the fake
  engine. The first real run against jet01 by hand is pending the user's go.
- **Agent image.** The workflow builds it, runs `claude --version`, `codex --version`, `opencode --version`
  (above the floors), `tmux -V`, `sshd -t`, checks the user is 10002 and `/` is read-only, generates a key,
  starts the container with it and logs in over ssh, runs the folder-trust Python script once, runs the
  canary command, and scans with trivy (fail on critical).
- **Security tests** (the LOOM-139 pattern): the pod spec and the container flags carry no `LOOMUX_*`, no
  token automount, no socket mount; a JSON-response scan for the host private key, the managed private key
  and every `x-secret` value on every plugin and target route; logs captured for key material and config
  secrets; label values from user text never reach object names; the subprocess's environment.
- **On TEST**: install the plugin from the UI (sidecar form), see the `not enforced` warning, create a small
  machine, provision a workspace by chat, run a Claude Code turn with a target-scoped token, stop/start,
  update after an image bump, uninstall with `keep` and reinstall, destroy; all four `test` steps green at
  each stage.

## 13. Open questions, each with a recommendation

1. **Plugin boundary** — out-of-process plugins from v1, first-party ones bundled in the image and also
   shipped as sidecar images *(recommended)* vs compiled-in behind an enable switch. §1.2.
2. **Protocol** — JSON-RPC 2.0 with `Content-Length` framing over stdio / a unix socket *(recommended)* vs
   gRPC (hashicorp `go-plugin`). §1.3.
3. **Distribution** — bundled binaries + signed release assets + plugin container images; no registry fetch
   at install in v1 *(recommended)* vs download-at-install now. §1.2.
4. **Trust** — bundled trusted as the image; non-bundled first-party verified by attestation; third-party
   needs an explicit confirmation that names its isolation (none as a subprocess, its own container as a
   sidecar) *(recommended)*. §1.6.
5. **Uninstall with targets** — refuse unless the user chooses destroy or keep *(recommended)* vs always
   destroy. §1.5.
6. **Transport** — sshd in the machine behind the managed-mode executor *(recommended)* vs a Kubernetes/
   Docker exec executor through the plugin. §2.1.
7. **Machine granularity** — one machine per target, workspaces as directories on it *(recommended)* vs one
   per workspace. §2.4.
8. **Namespace** — a separate `loomux-agents` namespace per instance, PSA `restricted` *(recommended)* vs
   agents in `loomux` itself (simpler RBAC, no isolation from loomuxd's secrets: rejected).
9. **Pod controller** — bare Pods reconciled by the host *(recommended)* vs a one-replica StatefulSet per
   machine.
10. **Agent image base and contents** — Debian slim + Node 22, the three CLIs pinned, python3, git, ripgrep,
    no compilers *(recommended; a `-full` variant with build tools can follow if agents keep asking)*. §3.
11. **Agent auth** — vault with a new `target_id` scope, plus a persistent home for a one-time interactive
    sign-in *(recommended both)* vs only one of them. §3.
12. **Egress and the policy engine** — internet allowed, cluster, RFC 1918, CGNAT, link-local and loopback
    denied once a policy engine exists; until then the plugin tests enforcement, warns, withdraws the egress
    choice and `require_network_policy` (default `false`, `true` for prod) decides whether machines may be
    made at all; theWyseKube installs kube-network-policies *(recommended)* vs refusing to create any
    machine on an unenforced cluster (safe, but TEST couldn't exercise the plugin while the engine is
    decided), or ignoring enforcement (rejected). §4, §8, §10.
13. **Docker credentials and transport** — the plugin's own SSH key and pinned host key, the Engine API over
    `docker system dial-stdio` *(recommended)* vs a TLS TCP endpoint with client certs, or sharing loomuxd's
    managed key (rejected: the plugin must not reach the host's keys). §9.
14. **Sandboxed runtime** — not in v1 *(recommended)*; a `runtime_class` plugin setting is a one-line
    addition when theWyseKube has gVisor or Kata.
15. **Router creating machines** — not in v1 *(recommended)*; `provision_target` behind a confirmation later.
    §7.
16. **Deleting a created machine** — cascade (workspaces, tasks, credentials, data) in one `DELETE`
    *(recommended)* vs keeping the rows and marking the machine destroyed (two steps, history kept). §5.
17. **PVC backups** — not in the nightly PBS set *(recommended)*; git remotes are the backup. §10.
18. **Kubernetes client in the plugin** — `client-go` *(recommended, a change from revision 1: the plugin is
    its own module, so its dependency weight no longer lands in the server)* vs a hand-written REST client.
    §8.
19. **Plugin configuration source** — the database, through the UI and API *(recommended)*, with an optional
    `LOOMUX_PLUGINS_SEED` JSON (applied once when the database has no such instance) for GitOps-style
    deployments as a follow-up if wanted, vs environment-only configuration (no UI). §1.5, §10.

## 14. Decisions for the user

Approving this revision with the recommendations above means: out-of-process plugins over JSON-RPC, bundled
in the image and offered as sidecar images, with no registry fetch yet (1–3); the trust model of §1.6 (4);
explicit destroy-or-keep on uninstall (5); sshd in the machine on the managed executor (6); machine per
target with one `/data` volume (7); a separate PSA-restricted namespace and bare pods (8, 9); the image as in
§3 (10); vault target scope plus persistent home (11); enforcement-tested egress with
`require_network_policy` and kube-network-policies on theWyseKube (12); the Docker plugin's own SSH key over
dial-stdio (13); no sandbox runtime, no router-made machines (14, 15); cascading delete (16); no PVC backups
(17); client-go in the plugin (18); plugin configuration in the database (19). Anything decided differently is
changed here before PR 1 starts, and the change is sent back to command-center as a handoff.
