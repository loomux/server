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
  first flagged).
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
