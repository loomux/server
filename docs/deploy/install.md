# Installing Loomux (LOOM-128)

A fresh deployment from nothing: what to prepare, how to configure and
run the server, and how to check it works. The reference behind each
step is linked; this page is the order to do them in.

Loomux is one process, `loomuxd`, shipped as the image
`ghcr.io/loomux/server` (public). It serves the web client and the API
on one port, keeps its state in one SQLite file, and reaches the
machines agents run on over SSH. It's single-user: one password.

## 1. What you need

- **A host for the server.** Kubernetes is what's tested (manifests
  below), but any container runtime works. It needs a persistent
  volume for the database and outbound HTTPS to your router-model API.
- **At least one target:** a machine where agents run, reachable from
  the server over SSH, with `tmux` and the agent CLIs (Claude
  Code ≥ 2.1.0, Codex ≥ 0.150.0) installed **and logged in** as the
  target user. Prepare it with [`targets.md`](targets.md).
- **A router model:** any OpenAI-compatible chat endpoint (base URL,
  API key, model name), plus optionally a second, escalation one. See
  the router section of [`container.md`](container.md#configuration).
- **HTTPS in front.** `loomuxd` speaks plain HTTP; put a reverse proxy or
  ingress that terminates TLS in front of it.
- Optional: an [ntfy](https://ntfy.sh) server and topic for
  notifications; Prometheus for metrics.

## 2. Pick a version

Releases are listed at <https://github.com/loomux/server/releases>.
Each is an image tag: `ghcr.io/loomux/server:0.1.N`, and the same image
as `:<commit sha>`. Pin one; don't run `:main`. Versioning is explained
in [`../release/versioning.md`](../release/versioning.md).

## 3. Make the secrets

Keep these in your secret store (a Kubernetes Secret, ideally encrypted
at rest, e.g. with SOPS). Never put them in a ConfigMap or the image.

| Variable | What it is | How to make it |
|---|---|---|
| `LOOMUX_AUTH_PASSWORD_HASH` | bcrypt hash of the one login password | `echo -n 'your password' \| docker run --rm -i ghcr.io/loomux/server:0.1.N -hash-password` |
| `LOOMUX_MASTER_KEY` | AES-256 key for the credential vault: 32 random bytes, base64. **Required**: without it every agent dispatch fails ("resolve credentials"), and run_command output and notification summaries are withheld, even though nothing fills the vault yet (agents use their own logins on the targets) | `head -c 32 /dev/urandom \| base64` |
| `LOOMUX_ROUTER_PRIMARY_API_KEY` | the router model's API key | from your provider |
| `LOOMUX_ROUTER_ESCALATION_API_KEY` | (optional) the escalation model's key | from your provider |
| `LOOMUX_NTFY_TOKEN` | (optional) ntfy publish token | from your ntfy server |

**Keep a copy of `LOOMUX_MASTER_KEY` with your backups.** Credentials
in the vault are encrypted with it; without it a restored database's
vault can't be read, and there's no way to change the key in place
(see [`operations.md`](operations.md#rotating-secrets)).

The SSH key and `known_hosts` for reaching targets are a separate
Secret, mounted as files: see [`ssh.md`](ssh.md).

## 4. Configure

Non-secret settings go in the environment (a ConfigMap). The full list,
with defaults, is in [`container.md`](container.md#configuration). The
ones a fresh install sets:

```
LOOMUX_DB_PATH=/data/loomux.db          # on the persistent volume (image default: /var/lib/loomux/loomux.db)
LOOMUX_ROUTER_PRIMARY_BASE_URL=https://…/v1
LOOMUX_ROUTER_PRIMARY_MODEL=…
LOOMUX_PUBLIC_URL=https://loomux.example # links in notifications
# optional
LOOMUX_ROUTER_ESCALATION_BASE_URL=…      # all three escalation vars, or none
LOOMUX_ROUTER_ESCALATION_MODEL=…
LOOMUX_NTFY_URL=https://ntfy.example
LOOMUX_NTFY_TOPIC=loomux
LOOMUX_METRICS_ADDR=:9090                # default is loopback only
```

Web client updates in place are off unless `LOOMUX_WEB_UPDATES` says
otherwise; a production install normally leaves them off, so the UI
changes only with the server image ([`container.md`](container.md)).

## 5. Run it

The image runs as uid 10001 with a read-only root filesystem if you
like; it needs these writable paths:

| Path | Why | Volume |
|---|---|---|
| directory of `LOOMUX_DB_PATH` (default `/var/lib/loomux`; the test instance mounts `/data`) | the database, and pre-migration snapshots if your setup takes them | **persistent** |
| `LOOMUX_WEB_BUNDLES_DIR` (default `web-bundles` beside the database) | web client bundles installed in place | **persistent** (the same volume) |
| `/tmp` | SSH ControlMaster sockets | `emptyDir` |
| `/home/loomux/.ssh` | the SSH Secret is copied here at start | `emptyDir` |

Container ports: 8080 (HTTP, web + API), 9090 (metrics, if enabled).
Probes: liveness, readiness and startup all on `GET /api/v1/health`
(unauthenticated: database and router model; details in
[`container.md`](container.md#health-probes-loom-105)). Any other
non-API path answers 200 with the web client, so don't probe one of
those.

**Reaching targets.** Targets on a Tailscale tailnet are reached
through a userspace Tailscale sidecar exposing a SOCKS5 proxy, with
the SSH config using it as `ProxyCommand` (no `NET_ADMIN`, no
`/dev/net/tun`). See [`ssh.md`](ssh.md). Targets on a routable network
need no sidecar.

**Kubernetes.** One replica only (SQLite, and in-memory state such as
offers awaiting a yes): `replicas: 1`, `strategy: Recreate`. A working
set of manifests (namespace, PVCs, ConfigMap, Deployment with the
Tailscale sidecar, Service, backups, ServiceMonitor and alert rules) is
what the test instance runs; its layout:

```
00-namespace.yaml          namespace
10-loomuxd-data.yaml       PVC for /data
10-loomuxd-backup.yaml     PVC for nightly backups
10-tailscale-state.yaml    PVC for the sidecar's state
20-loomuxd-config.yaml     ConfigMap (section 4)
30-loomuxd.yaml            Deployment: init container (pre-migration
                           snapshot), loomuxd, Tailscale sidecar
40-loomuxd.yaml            Service + ingress
50-servicemonitor.yaml     Prometheus scrape
51-prometheusrules.yaml    alerts
60-backup-cronjob.yaml     nightly online SQLite backup
secrets/                   SOPS-encrypted Secrets (section 3, SSH)
```

**Plain Docker**, for a try-out:

```
docker run -d --name loomuxd -p 8080:8080 \
  -v loomux-data:/var/lib/loomux \
  --env-file loomux.env \
  -v "$PWD/ssh:/etc/loomux/ssh:ro" \
  ghcr.io/loomux/server:0.1.N
```

Mount the volume at the image's own `/var/lib/loomux` and keep the
default `LOOMUX_DB_PATH`: that directory belongs to uid 10001, and
Docker copies its ownership into a fresh named volume. A volume
mounted at a path the image doesn't have (say `/data`) comes up owned
by root and `loomuxd` can't create the database. The files under
`./ssh` must be readable by uid 10001 (the entrypoint copies them).
Plain Docker takes no pre-migration snapshots (on Kubernetes the
test instance's init container does): copy the database off the
volume yourself before an upgrade.

## 6. Check it

1. `curl https://loomux.example/api/v1/version` →
   `{"server_version":"0.1.N (web 0.1.M)","api_version":"v1"}`.
2. `curl https://loomux.example/api/v1/health` →
   `{"status":"healthy",…}`.
3. Open the web UI, log in with the password from section 3.
4. **Targets → Register target**: name, host, user (and its policy:
   purpose, allowed agents, whether new work needs a confirmation).
   The health probe reports whether SSH, tmux and the agent CLIs are
   there.
5. **New conversation**: ask something answerable directly, then
   "run \`hostname\` on <target>" (runs at once and replies with the
   output), then a small agent task ("in a new workspace demo, create
   hello.txt containing hi").
6. If you set ntfy up, a turn that took longer than 30 s notifies.

## 7. Then

- Set up backups and **try a restore once**: [`backup-restore.md`](backup-restore.md).
- Day-to-day operation: [`operations.md`](operations.md).
