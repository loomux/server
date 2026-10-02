# router

Router-model integration. See design spec §6.

A single swappable configuration setting, not hardcoded to a vendor.
Responsible for routing (workspace select / new-workspace provisioning /
direct answer, against compact registry metadata — not full workspace
history) and relay (condensing captured agent output into chat-appropriate
replies, and updating each workspace's rolling summary).

## Layout

- `agenttype.go` — `AgentType`/`AgentTypeRegistry`: the full per-agent-
  type registration spec §6 describes (a launch command template plus a
  completion-detection adapter), composing `completion.AgentConfig`
  rather than duplicating it. `CompletionConfig()` always derives
  `completion.NewDetector`'s config from this registry — one source of
  truth. An entry keyed by `""` configures completion detection for
  shell-kind (provisioning) tasks, since `registry.Task.AgentType` is
  always empty for those.
- `availability.go` / `confirm.go` — agent availability (LOOM-71): the
  pre-launch `command -v` probe recorded per target (`registry.
  TargetAgent`), `RefreshTargetAgents`, and the install offer: shown with
  its exact command, run as a `command` task only when the conversation's
  next message is an explicit, deterministically matched "yes" (design
  spec §6 "Agent availability and install offers"). Also the bounded,
  credential-redacted quoting of process output used in replies/errors.
- `command.go` — direct shell commands (`ActionRunCommand`, LOOM-72):
  `parseRunRequest`/`orderedVerbatim` (run at once only if the whole
  message is ``run `<cmd>` on <target>`` — or the fenced form — naming the
  router's exact command and target; otherwise confirm first, showing
  both, via the same pending-offer mechanism as installs), the per-target `shell@<target>` workspace, and
  the verbatim, bounded, vault-redacted output reply.
- `routing.go` — `RoutingModel` (the swappable seam), `Decision`,
  `WorkspaceSnapshot`, `ProvisionSpec`, `RelayResult`.
- `router.go` — `Router`, the actual composition: `Dispatch` routes a
  message, resolves or provisions a workspace, then `dispatchToAgent`
  finds the task already open for that workspace + conversation
  (`findActiveTask`) or launches a fresh one, sends the message into it
  (`orchestrator.SendMessage` — uniformly, first turn and follow-ups
  alike), waits for completion, relays the captured output, and applies
  the result: `RelayResult.Done` decides whether the task is torn down
  via `orchestrator.Complete` or left open (`registry.
  TaskStatusAwaitingInput`) for the next turn in the same session
  (design spec §3 steps 2-3, LOOM-13 — closes the gap LOOM-5's handoff
  first flagged). Before reusing a found active task, `sessionIsLive`
  checks it actually still has a live tmux session — a task can be
  legitimately open (`AwaitingInput`) but sessionless if the idle reaper
  (`orchestrator.Reaper`, LOOM-16) tore it down, or a crash/manual kill
  did; either way `dispatchToAgent` fails the stale task (so
  `findActiveTask` stops finding it) and transparently falls back to a
  fresh launch rather than erroring the conversation out. `launchAgent`
  mints the task's ID itself, before building the launch command, and
  passes it to `orchestrator.LaunchWithID` rather than letting `Launch`
  mint one afterward — see the TierMarker section below (LOOM-32).
- `version.go` — `VersionCheck`, `ExtractDottedVersion`, `CheckVersionRange`
  (design spec §10 axis 3, LOOM-17): a per-agent-type declared version
  gate, checked at launch. See the Agent-adapter versioning section below.
- `routertest/` — `StubRoutingModel`, a plain call-and-return test
  double (not signaled/blocking, unlike
  `orchestrator/detectortest.ManualDetector` — `Decide`/`Relay` aren't
  "wait until told" operations).
- `llmrouter/` — the real, LLM-backed `RoutingModel`: a generic
  OpenAI-Chat-Completions-compatible client with a config-swappable
  primary tier (intended to be a free/cheap, fast model — routing and
  relay are simple extraction/summarization, not hard reasoning) and an
  optional escalation tier for when the primary's output is unusable or
  the primary is unavailable. See `llmrouter/README.md`.

Run `go test ./...` from the repo root to run the full suite, including
one true end-to-end integration test (real local tmux, real tiered
completion detection, real credential resolution), `continuation_test.go`
(the multi-turn task-reuse cases, including a human-takeover refusal and
an ambiguous-active-task guard), the security-relevant test proving a
resolved secret's raw value never appears in captured pane output, the
relayed reply, or the rolling summary, and `tiermarker_integration_test.go`
(a real-tmux end-to-end proof that TierMarker actually works — see
below).

## Getting a task ID into a launched agent (LOOM-32)

TierMarker (`completion.TierMarker`) was declared for both
`app.DefaultAgentTypes()` entries (`"claude-code"`, `"codex"`) from the
day the Codex adapter shipped, but was nonfunctional: nothing told a
launched process its own task ID, so a hook/notify script running inside
it had no way to know which marker file to touch. The gap was
structural — `orchestrator.Launch` minted `task.ID` *after* the caller
had already built the final launch command.

Fixed by inverting the order in `launchAgent`: it now mints the task ID
itself (`uuid.NewString()`) before building `command`, and passes both
to the new `orchestrator.LaunchWithID` (`Launch` is now a thin wrapper
around it for callers — e.g. `provisionWorkspace`'s shell-kind tasks —
that don't need to know the ID up front). `agentEnvPrefix` then builds a
`VAR='value' ` shell prefix (reusing `credentials.ShellEnvPrefix` — a
generic, already-tested "map → safe shell env prefix" utility, not
credential-specific in its mechanism) containing:

- `LOOMUX_TASK_ID` — always, regardless of tier. Cheap, and generically
  useful to any hook needing task-scoped behavior beyond just markers.
- `LOOMUX_MARKER_PATH` — only when `entry.Tier == completion.TierMarker`,
  computed via `completion.MarkerPath(dir, taskID)`. `dir` is
  `r.markerDir` (the configured `LOOMUX_MARKER_DIR`), or, when that is
  empty, the per-user `0700` default that `completion.ResolveMarkerDir`
  creates on the target. `app.build` passes the *same* configured value
  to both the real `completion.Detector` and `Router`, and both resolve
  an empty one with the same function — a
  hook told to touch a path the Detector never watches would be a silent
  no-op, so this single-resolution discipline is what keeps them
  necessarily in agreement.

## Injecting the completion hook at launch (LOOM-75)

The env var alone did nothing: no target had a hook that reads it
(probed 2026-10-02 — none on sc1, no agent config at all on jet01), so
every TierMarker turn would have waited forever. Loomux now owns the hook
too. `AgentType.CompletionHookArgs` are appended to `LaunchTemplate` on
every launch (`AgentType.launchCommand`, each argument shell-quoted as
one word), and the adapters in package `agents` set them:

- `claude-code`: `--settings '{"hooks":{"Stop":[…]}}'`. Claude Code layers
  `--settings` over the user's own settings for this process only, and
  runs hooks from every layer, so the user's own hooks still run.
- `codex`: `-c notify=["sh","-c","…","loomux-notify"]`. The script touches
  the marker only for `agent-turn-complete` events. This replaces the
  user's own `notify` for that process (Codex has a single notify
  program).

Both hooks `mkdir -p` the marker's directory first (nothing creates it on
a target), and do nothing when `LOOMUX_MARKER_PATH` is unset. The user's
agent config files on the target are never read or written; targets can
be shared work hosts.

## Launch profiles (LOOM-78)

`AgentType.Profile` (`router.LaunchProfile`) adds three things to the
command line, in this order: `<template> <permission args> <trust args
for ws.Path> <completion hook args> -- <first message>`
(`AgentType.launchCommand`; every argument is shell-quoted as one word).
When `PromptAsArg` is set, `launchAgent` puts a fresh task's message on
the command line and `dispatchToAgent` skips `SendMessage` for that turn.
Turns after the first are always typed in. `ApplyProfileOverrides`
applies the operator's `LOOMUX_AGENT_PROFILES`. The defaults, and what
each one permits, are in `agents/README.md`.

## Agent-adapter versioning (design spec §10 axis 3, LOOM-17)

`AgentType.VersionCheck` (nil means "no check enforced") declares a
command (`targets.TargetExecutor.RunOnce` — a real one-shot "run this,
read the output" primitive, distinct from the tmux-pane-oriented session
methods), a `Parse` function extracting a comparable dotted-number
version string from that command's raw output, and a `[Min, Max)` range.
`router.launchAgent` runs the check — if one is declared — before
anything else, including credential resolution: a failing check means
`orchestrator.Launch` is never called at all, so no task record gets
created for a doomed launch. That's "fails loud at launch, not silently
mid-task via a broken completion signal" (the spec's own words) made
concrete: the dispatch attempt itself fails.

`Parse` is per-agent-type rather than one shared regex, because each
CLI's `--version` output has its own shape — `claude --version` prints
`2.1.251 (Claude Code)`, not strict semver. `ExtractDottedVersion` is a
reusable `Parse` for the common "grab the first dotted-number sequence"
case; `CheckVersionRange`/`compareVersions` do the actual comparison
(component-wise on dotted numbers — not full semver, no pre-release/
build metadata, which is more than this needs).

**Wired into both production adapters since LOOM-75.** The injected
completion hook gave the first concrete reason for a floor: a CLI that
lacks the hook flag would leave the marker wait hanging. `agents` sets
`Min` to the oldest release each launch was verified against with a
real turn (claude 2.1.x, codex 0.150), and `VersionCheck.Requires`
names the feature, so a below-minimum CLI fails the dispatch with
"agent version too old for completion hooks (…)". The error wraps
`ErrVersionTooOld`.

Whether to cache a version check per target+agent-type (avoiding a fresh
`RunOnce` — an SSH round-trip for a remote target — on every single
launch) or just re-run it every time is left as-is: re-run every time.
`RunOnce` is cheap enough that this probably doesn't matter much either
way; caching can be added later if it ever does.
