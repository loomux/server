# Plugins (LOOM-178)

A plugin extends Loomux with something that needs infrastructure
credentials and infrastructure-specific code: the first kind creates
machines for agents on demand (a Kubernetes pod or a Docker container
per target). Plugins are opt-in per deployment, installed from the web
UI or `POST /api/v1/plugins`. Without one, Loomux works exactly as before:
you register the hosts you already use. Design and threat model:
`docs/design/target-providers.md`.

## Where loomuxd looks

| Setting | Image default | What is there |
|---|---|---|
| `LOOMUX_PLUGIN_BUNDLE_DIR` | `/usr/local/lib/loomux/plugins` | first-party plugins shipped in the image, one directory each with `plugin.json` and `loomux-plugin-<name>`. Trusted as the image is. The Kubernetes plugin (`plugins/kubernetes/README.md`) and the Docker plugin (`plugins/docker/README.md`) are bundled. |
| `LOOMUX_PLUGIN_DIR` | `plugins/` beside the database | plugins you put there yourself, same layout. Shown as **unsigned** until signed first-party assets exist; installing one means trusting it with the permissions its manifest declares |
| `LOOMUX_PLUGIN_SOCKET_DIR` | `/run/loomux/plugins` | sockets served by sidecar containers (`loomux-plugin-<name> --listen /run/loomux/plugins/<name>.sock`). The recommended form on Kubernetes: the plugin container alone holds the cluster credential |

`GET /api/v1/plugins/available` is the catalog of all three.

## What a plugin runs as

A plugin from a directory runs as a **subprocess of loomuxd, as
loomuxd's own user**, with a clean environment (`PATH`, `HOME`,
`TMPDIR`, the locale; never `LOOMUX_*`). That isolates crashes and
dependencies, not secrets: a same-user process can read loomuxd's
database and environment. For a plugin you don't fully trust, run it as
a sidecar instead.

A sidecar plugin runs in its own container with its own user and
mounts; loomuxd only reaches its socket. On Kubernetes, give the pod the
plugin's ServiceAccount with `automountServiceAccountToken: false` and
project the token into the plugin container only.

## Installing and configuring

Install needs `LOOMUX_MASTER_KEY`: secret settings (`x-secret` fields
of the manifest's schema) are encrypted with it, like the vault and the
managed SSH keys, and never returned by the API. The install dialog
shows the plugin's permissions, its trust and its isolation first.

A plugin that starts but can't work (its check fails) stays installed
in status `error` with the reason, so you can fix its configuration;
`PUT /api/v1/plugins/{id}/config` reconfigures it in place. **Disable**
stops it and keeps everything; its machines keep working as ordinary
targets, only their start/stop/recreate/destroy pause. **Uninstall**
asks what to do with its machines: destroy them (data included) or keep
them as plain hosts.

## Declaring plugins in the deployment (`LOOMUX_PLUGINS_SEED`)

For a GitOps deployment, the plugin instances can be declared instead
of installed by hand (LOOM-219). `LOOMUX_PLUGINS_SEED` holds the JSON,
or the path of a file with it (a mounted ConfigMap):

```json
{
  "prune": false,
  "plugins": [
    {
      "label": "wysekube",
      "plugin": "kubernetes",
      "source": "socket",
      "config": {"namespace": "loomux-agents", "storage_class": "ceph-rbd-sc-delete"},
      "secrets": {"kubeconfig": {"file": "/run/secrets/loomux-plugin/kubeconfig"}}
    }
  ]
}
```

`plugin` and `source` (`bundled`, `dir` or `socket`) are what the
catalog lists; `config` is the configuration as the install form takes
it, without its secret fields. Those are never written in the seed: each
is a reference in `secrets`, to an environment variable (`{"env":
"NAME"}`) or a file (`{"file": "/path"}`, its trailing newline dropped),
read when the seed is applied. A secret field in `config` is refused.

After the installed plugins have started, loomuxd logs the plan on one
line (`plugins seed plan`) and applies it:

- an instance the seed lists and none exists with that label is
  installed, as `POST /api/v1/plugins` would;
- one the seed installed is reconfigured when the seed says otherwise
  (a secret's new value counts; a secret dropped from the seed is
  cleared), and left alone when nothing changed;
- one installed through the UI or API is never touched, even with a
  label the seed lists: the log says so. Which instances are the seed's
  is kept in the database (the `plugins_seed_owned` setting, their ids);
- one the seed installed and no longer lists is kept, unless the seed
  says `"prune": true`: it is then uninstalled, but only if it owns no
  machines. Destroying machines is never the seed's to do: uninstall
  such an instance through the API, choosing to destroy or keep them.

The seed is the source of truth for its own instances: a change made to
one through the UI is undone at the next start if the seed differs.

A malformed seed (not JSON, an unknown field, a label twice, a secret
both inline and referenced) stops loomuxd at startup, like any malformed
setting, before anything is applied. A problem only applying reveals (a
plugin not in the catalog yet, a missing environment variable or file, a
check that fails) is logged per instance and loomuxd runs on; an entry
whose plugin isn't there yet (a sidecar still starting) is tried again
every 30 s for 10 minutes. `LOOMUX_PLUGINS_SEED_DRY_RUN=true` logs the
plan and applies nothing.

## Upgrades and compatibility

A bundled plugin's available version changes with the image; a
directory plugin's when you replace its files; a sidecar's with its
container. `GET /api/v1/plugins` shows `version` next to
`available_version`; `POST /api/v1/plugins/{id}/upgrade` starts the new
version beside the old one and checks it before switching: if it can't
start or fails its check, the old version keeps running and the row
keeps its version (`422` with the reason). It is refused up front if
the new version drops a capability the plugin's machines rely on.

**Bundled and sidecar plugins follow their image without an upgrade.**
First-party plugins are built with the server and carry its version
(`docs/release/versioning.md`, "Images"), so every server release
bundles a new plugin version, and a sidecar's version changes with its
container. At startup the server adopts the new version on its own,
for a bundled plugin and for a sidecar (from what it describes itself
as), when it declares the same capabilities and configuration schema
as the installed one, defaults aside: the row moves to the new
version and the new binary runs, nothing to click. A setting still
equal to the plugin's previous default follows its new default at that
point (the `agent_image` pin of a release, for instance); one that
differs from the previous default stays. The server can't tell a value
you set from the default it equals, so to freeze an image, pin a value
other than the one the plugin shipped with. A sidecar that wasn't
answering when the server started is in status `error`; enabling it
again adopts it the same way once it is up. When the capabilities or
the schema's shape changed, the plugin stays stopped in status `error`
with a reason naming the new version ("upgrade the plugin"): the old
binary is no longer there, and only `POST /api/v1/plugins/{id}/upgrade`,
which applies the same default rule, adds new settings' defaults and
checks the new version, can move it. A directory plugin is yours to
upgrade: replace its files, then press Upgrade.

**`min_host_version` is enforced.** A manifest's `min_host_version` is
checked against the server's version. The server **refuses** a plugin
when it is below that minimum (`0.3.81` or `0.4.0` on a `0.3.80`
server) or on another major (from 1.0 the major is the compatibility
unit; a `1.x` plugin on a `2.x` server, or a `0.y` one on a `1.x`
server): `GET /api/v1/plugins/available` still lists it, with
`incompatible` saying why; installing or upgrading to it answers `409`,
and an installed one stays stopped in status `error` with the same
reason. A plugin built for an **older `0.Y`** than the server's
(`min_host_version 0.3.0` on a `0.4.2` server) is listed with
`incompatible_warning` instead and a warning is logged, but it installs
and runs: under 0.x the minor is where things change, so it may not
match, and the message says so. A server built without a release version
(`dev`, a plain commit build) runs anything. Bundled plugins always
match, since they are stamped with the server's own version; the rule
matters for a sidecar or a directory plugin from another release.

Whatever a plugin writes (its stderr, an error message, a check
problem) is scrubbed of the plugin's own secret settings before Loomux
stores, shows or logs it.

## Machines a plugin makes

A plugin that declares `targets.create` offers **Create a machine** on
the Machines page (and `POST /api/v1/targets` with a `plugin` object).
Loomux generates the target's SSH key and the machine's host key, pins
the host key before the machine exists, and registers the machine as an
ordinary managed remote target (user `agent`, workspace root
`/data/work`). The machine comes up in the background; the target shows
`plugin.status` (`creating`, `starting`, `running`, `stopped`, `error`,
`lost`, `detached`) next to the usual health. Stop keeps a persistent
machine's data and frees its compute; start brings it back at the same
address; recreate applies the plugin's configured image; deleting the
target destroys the machine, its data, its workspaces and the
credentials scoped to it. A dispatch to a stopped machine starts it
first. Loomux reconciles with the plugin on the probe interval: a
machine left creating by a restart is resumed, a persistent one the
plugin lost is made again, an ephemeral one is marked lost and its
workspaces archived, and machines the plugin has that Loomux doesn't
know are destroyed after a day.

Credentials for the agents on a machine go in the vault scoped to the
target (`target_id`); a one-time interactive sign-in kept in the
machine's home works too, where the plugin keeps the data.

### The Docker plugin

A container per machine on one Docker host, reached over SSH at a port
published on the host's address (`bind_address`: its tailnet IP, or
`127.0.0.1` when loomuxd runs on the docker host itself). The plugin
reaches the engine over SSH too, as a user in the docker group, with
its own key (**Generate** in the install form, then add the public half
to that user's `authorized_keys`), or through the engine's local
socket. **The docker group is root on that host** and the plugin's
manifest says so; it uses a fixed set of engine endpoints, listed in
its README, and labels everything it makes.

The first check reports `host_key_unpinned` with the host's key type,
fingerprint and the line to trust: put it in `ssh_host_key` and check
again. Until then the plugin never connects.

What Docker can't give: no network isolation (a machine can reach the
LAN and the tailnet its host is on; the plugin offers no `egress`
choice, since Docker publishes no port on an internal network), no
quota beyond each container's limits (`max_environments` is the cap),
no admission control (the plugin asserts its own container flags, and
a test pins them). The docker host's own security is yours.

## Writing a plugin

Implement `plugins/sdk.Plugin` and call `sdk.Main`; ship `plugin.json`
beside the executable `loomux-plugin-<name>`. `plugins/fake` is a
complete example, and `plugins/plugintest.Run` is the conformance suite
yours must pass. The protocol is JSON-RPC 2.0 with `Content-Length`
framing (the LSP/MCP convention), so a plugin can be written in any
language; the methods are in `plugins/protocol`.
