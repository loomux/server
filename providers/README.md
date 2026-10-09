# providers

The host side of the target-provider capability (LOOM-178,
`docs/design/target-providers.md` §1.9, §2): a plugin that declares
`targets.create` makes machines on demand, and this package turns each
into an ordinary managed remote target. Nothing upstream of the target
knows the difference: the executor, completion markers, health probes,
the orphan sweep and attach-info all see an SSH target.

## What `Create` does

1. Checks the plugin is installed, enabled and running, and that what
   it offers (`targets.describe`: sizes, persistence, egress, the sshd
   user and port, the address template) allows the request.
2. Generates the target's own SSH key (origin `target`, like
   `generate_ssh_key`) and the machine's **host key**: ed25519, the
   private half stored encrypted (`environments.host_private_key`, AAD
   `environment:<id>`), the public half **pinned on the target before
   the machine exists**, so managed mode's no-pin-no-connection rule is
   met from the first contact and there is no scan or TOFU window.
3. Writes the target row (`kind: remote`, managed, user and port from the
   plugin, `workspace_root: /data/work`, `ssh_proxy` per the plugin) and
   the environment row (`status: creating`), in that order, so a crash
   leaves rows that startup resumes, never an unowned machine.
4. Asks the plugin (`targets.create`, idempotent on the environment id)
   in a background job bounded by 10 minutes, follows the machine to
   `running` (`targets.get`), takes its reported address (moving the
   target's host, port and pin if the plugin's differs from the
   template's), and probes the target.

Every answer from a plugin is validated: addresses against the managed
host grammar, statuses against the fixed list, strings bounded.

## Lifecycle

`Stop` (persistent machines only; refused while a task is mid-turn or
taken over), `Start` (a lost or errored machine is made again with the
same id, key and host key), `Recreate` (the plugin's configured image,
optionally another size; same data and address), `DeleteTarget` (the
cascade of design §5: workspaces with their tasks, target-scoped
credentials, the machine through the plugin, the generated key, the
rows), `EnsureRunning` (what the router calls before dispatching to a
stopped machine), `AttachCommands`.

## Reconcile

At startup and on the probe interval, against `targets.list` and
`targets.get`: a `creating` machine is resumed; a persistent machine the
plugin lost is made again; an ephemeral one is `lost` and its workspaces
archived; a `stopped` machine found running is stopped; a `destroying`
one is finished. Machines the plugin has for this instance that no row
knows are orphans, destroyed once older than 24 h. A plugin's
`targets.changed` notification reconciles that machine sooner.

`UninstallMachines` serves the plugin manager's uninstall choice:
destroy each machine, or keep them as `detached` plain hosts.

## Layout

- `providers.go` — errors, ids, host-key and pin helpers, the address
  grammar, `ActiveTaskChecker`.
- `manager.go` — `Manager`: create, the background jobs, the lifecycle,
  the cascade, views for the API.
- `reconcile.go` — the reconcile table and the orphan sweep.

Tests build `cmd/loomux-plugin-fake` once and drive a real plugin
manager over it.
