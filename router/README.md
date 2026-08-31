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
  fresh launch rather than erroring the conversation out.
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
an ambiguous-active-task guard), and the security-relevant test proving a
resolved secret's raw value never appears in captured pane output, the
relayed reply, or the rolling summary.

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

**Deliberately not wired into `app.DefaultAgentTypes()`'s production
`"claude-code"` entry.** The mechanism is built and verified against the
real `claude` binary (`router/version_check_integration_test.go`,
running the actual installed CLI, not a fake), but picking the actual
supported version floor for production is a policy call this ticket
didn't have grounds to make — it's only ever been checked against the one
version installed in this environment. Wiring in an unverified `Min`
would either silently block real usage (set too high) or provide false
confidence (set too low, or matching only what happens to be installed
right now). Worth its own decision once there's real signal about what
range Loomux actually needs to support.

Whether to cache a version check per target+agent-type (avoiding a fresh
`RunOnce` — an SSH round-trip for a remote target — on every single
launch) or just re-run it every time is left as-is: re-run every time.
`RunOnce` is cheap enough that this probably doesn't matter much either
way; caching can be added later if it ever does.
