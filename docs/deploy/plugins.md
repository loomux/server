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
| `LOOMUX_PLUGIN_BUNDLE_DIR` | `/usr/local/lib/loomux/plugins` | first-party plugins shipped in the image, one directory each with `plugin.json` and `loomux-plugin-<name>`. Trusted as the image is. (None yet: the Kubernetes and Docker plugins follow.) |
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
`available_version`; `POST /api/v1/plugins/{id}/upgrade` switches, and
refuses if the new version drops a capability the plugin's machines
rely on.

## Writing a plugin

Implement `plugins/sdk.Plugin` and call `sdk.Main`; ship `plugin.json`
beside the executable `loomux-plugin-<name>`. `plugins/fake` is a
complete example, and `plugins/plugintest.Run` is the conformance suite
yours must pass. The protocol is JSON-RPC 2.0 with `Content-Length`
framing (the LSP/MCP convention), so a plugin can be written in any
language; the methods are in `plugins/protocol`.
