# Per-target agent launch args: model and effort (LOOM-307)

Status: design note for PR "LOOM-307", 2026-11-20. Closes Loomux/server#545.

## Why

command-center dispatches to sc1 (a shared, work-only host) with a pinned model
and effort: never Fable, never max effort there. Loomux can't pin either. Each
agent type's launch is fixed in `agents/agents.go`: Claude Code gets its Stop
hook, folder trust and the target's permission mode, Codex its notify hook,
trust and permission flags, and neither gets a model or an effort. A Loomux
dispatch to sc1 would run on the account's defaults, so sc1 stays on the
laptop's tmux path until this exists (decision D3, 2026-11-19; readiness gap
G15).

## What exists

- `registry.Target.PermissionMode`: the one per-target launch knob today. Set
  through `POST/PUT /api/v1/targets`, stored in `targets.permission_mode`,
  mapped to each CLI's own flags by `router.LaunchProfile.PermissionModes`.
- `router.LaunchProfile` + `LOOMUX_AGENT_PROFILES` (`ApplyProfileOverrides`):
  a per-deployment, per-agent-type override of permission args, pre-trust and
  prompt-as-arg. Read once at start; the same for every target.
- `router.AgentType.launchCommand`: builds the argv (template, permission
  args, trust args, `StartupArgs`, `CompletionHookArgs`, `--` prompt), each
  argument shell-quoted. The router never lets a model or a prompt add flags.

## Per target, not per profile

The setting is **per target, keyed by agent type**. A profile is per
deployment: one Loomux instance drives sc1 and personal hosts at once, and
the pin is a property of the host (its owner's rules), exactly like
`PermissionMode`, which is per target for the same reason. A per-target
setting is stored in the registry, changed through the target API without a
restart, and reaches provider-made targets the same way as registered ones. A
per-deployment default (an `LOOMUX_AGENT_PROFILES` `model`/`effort`) could be
added later on top; it isn't needed for sc1.

## Shape

```go
// registry
type AgentArgs struct {
    Model  string // "" = the CLI's / account's default
    Effort string // "" = the CLI's default
}
type Target struct { ...; AgentArgs map[string]AgentArgs } // key: agent type
```

API (`targetRequest` / `targetResponse`), snake_case like the rest:

```json
"agent_args": {
  "claude-code": {"model": "opus", "effort": "high"},
  "codex":       {"model": "gpt-5.1-codex", "effort": "medium"}
}
```

- Create: optional; absent or `{}` means no pins.
- Update (`PUT`, partial since LOOM-253): absent keeps the stored map;
  present replaces the **whole** map (so a client can remove a pin by
  omitting its agent type). An entry with both fields empty is dropped.
- `GET` shows the stored map (`{}` when none).
- `attach-info` gains `agent_args: {"model": "...", "effort": "..."}`:
  what **this task** was launched with, recorded on the task at launch, so a
  pin changed afterwards doesn't misreport a running pane. Empty strings when
  the launch pinned nothing (account defaults).

Each adapter in `agents` maps the two values to its CLI's own flags, beside
its `PermissionModes`:

| agent type  | model             | effort                                   | allowed efforts                    |
|-------------|-------------------|------------------------------------------|------------------------------------|
| claude-code | `--model <m>`     | `--effort <e>`                           | low, medium, high, xhigh, max      |
| codex       | `-m <m>`          | `-c model_reasoning_effort="<e>"` (TOML) | minimal, low, medium, high, xhigh  |

(Claude Code 2.1.329 `--help`; Codex 0.163.0 `-m`, `-c` parses TOML.) An agent
type whose adapter has no mapping refuses any pin for it.

## Validation (strict, fail closed)

- Agent type key: a registered agent type (API: 400 otherwise). The registry
  checks only that keys are non-blank and unpadded; the API and the launch
  check them against the agent registry.
- `model`: 1-64 chars, `^[A-Za-z0-9][A-Za-z0-9._:-]*$`, optionally followed by
  Claude Code's `[1m]` context suffix (`sonnet[1m]`). No spaces, no leading
  `-` (it would read as a flag), no `=`, quotes, `$`, `;`, `|`, `&`, backticks,
  globs or newlines. The value is a single argv element after a fixed flag,
  and is shell-quoted anyway; the allow-list is defence in depth and keeps
  the stored value boring.
- `effort`: one of that agent type's allowed values, lower case.
- Launch re-checks: a stored pin the adapter doesn't accept (an agent type
  whose allowed efforts shrank after an upgrade) **fails the launch** with a
  clear error. It never falls back to the account's defaults, since running
  unpinned is exactly what the pin exists to stop.

## Who can set it

Only the target API, behind the same admin auth as `permission_mode`.

- The router models never see or choose launch args: `Decide` returns an
  agent type and a message, and the launch reads the pin from the target.
- A prompt can't add flags: the prompt is passed after `--`, the pin is read
  from the store, and no other input reaches the argv.
- Seeds (`LOOMUX_PLUGINS_SEED` / provider seeds) don't set it in this PR; a
  provider-made target gets pins through the API like any other target.

## Storage

Migration `00039_target_agent_args.sql`:

```sql
ALTER TABLE targets ADD COLUMN agent_args TEXT NOT NULL DEFAULT '{}';
ALTER TABLE tasks   ADD COLUMN agent_model  TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks   ADD COLUMN agent_effort TEXT NOT NULL DEFAULT '';
```

`targets.agent_args` is JSON (`{"claude-code":{"model":..,"effort":..}}`),
the map being small, read with the target and never queried by field. Existing
rows read as no pins, so nothing changes for a target until someone sets one.
(00039, not 00038: draft PR #558, LOOM-316 agent sign-in, adds 00038. Goose
refuses a lower version after a higher one is applied, so whichever of the two
merges second must carry the higher number.)

## Deny-list (considered, not built)

An optional per-target deny-list (sc1: no Fable, no max effort) would refuse a
pin that names a denied model or effort. **Recommendation: no, not now.** The
pin and a deny-list would be set by the same admin through the same API, so
the list adds no separation of duty; nothing else in Loomux picks a model (the
router can't, a prompt can't), so there is no second chooser to guard
against; and a person attached to the pane can `/model` past any list, which
Loomux can't stop either way. Revisit if a later change lets the router or a
chat pick the model per turn: then a per-target allow/deny list becomes the
guard and belongs with that change.

## Not in scope

- A person who attaches can still change the model inside the session
  (`/model`, `/effort`). Loomux pins what it launches; it doesn't police the pane.
- Turns after the first reuse the running session, so a pin changed while a
  task is open applies from the next fresh launch.
- Per-deployment defaults, seeded pins, the web/TUI clients' forms.
