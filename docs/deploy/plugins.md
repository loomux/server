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

## Upgrades

A bundled plugin's available version changes with the image; a
directory plugin's when you replace its files; a sidecar's with its
container. `GET /api/v1/plugins` shows `version` next to
`available_version`; `POST /api/v1/plugins/{id}/upgrade` starts the new
version beside the old one and checks it before switching: if it can't
start or fails its check, the old version keeps running and the row
keeps its version (`422` with the reason). It is refused up front if
the new version drops a capability the plugin's machines rely on.

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
