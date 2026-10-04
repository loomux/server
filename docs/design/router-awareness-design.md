# Router situational awareness (LOOM-88 + LOOM-87)

**Status:** approved 2026-10-04 (substitution chosen; evals run against the deployed router-model key) · **Tickets:** LOOM-88 (Vikunja Loomux #1207, GitHub #89), LOOM-87 (#1206, GitHub #88)
**Source:** command-center `reports/2026-10-02-loomux-dispatch-failure-modes.md`, findings B1–B3, proposals #13/#14
**User feedback after the LOOM-80 deploy:** "I can't say it's aware too much of what it's doing."

## What main already has

Read at `831176a`:

- Each `TargetSnapshot` already carries `Agents map[string]bool` from the LOOM-71 probe, rendered as
  "claude-code: available, codex: not installed"; the system prompt says to prefer an available agent.
- `snapshotWorkspaces` already drops `failed` and shell (LOOM-72) workspaces. It does **not** drop `archived`.
- The probe (`requireAgent`) already runs before any workspace or task row is written, on both the
  provision path (`act`) and the launch path (`launchAgent`). An absent CLI becomes an install offer.
- No routing eval harness exists. Decide is tested only with a stub HTTP model.

## Dependencies

- "Persist task failure reason / status transitions" = **LOOM-77, merged**.
- "Target health probe" = **LOOM-86, open**. Not needed for this slice: agent availability (the part
  LOOM-88 actually needs) comes from LOOM-71's probe records, which exist today. Reachability, disk, login
  state and versions stay LOOM-86's; once it lands they become more rows in the same target block.
- "Policy tags" = **LOOM-89** (targets have no tags field yet). Left out; the target block gets them when
  LOOM-89 adds the field.

## Shape: one server PR, one web PR

Both tickets rewrite the same `decideUserPrompt`, the same snapshot types and the same eval suite. They are
also only worth evaluating together: the token budget and the affinity rule interact with the enriched
workspace list. So: **one server PR** (two commit series, `LOOM-88:` then `LOOM-87:`), closing #89 and
"Part of #88"; **one web PR** in loomux/web for the workspace_hint, which closes #88's web half.

## LOOM-88: snapshot enrichment

### Workspace snapshot

```go
type WorkspaceSnapshot struct {
    ID, Name, Description string
    Tags, Capabilities    []string
    Status     string     // idle | active | provisioning
    TargetName string     // the target's name, never its host (LOOM-64 rule)
    Summary    string     // RollingSummary, truncated to 240 runes + "…"
    LastUsed   *time.Time // rendered relative: "3h ago", "never"
}
```

Excluded: `failed` (as today), **`archived`** (new), shell workspaces (as today). Ordered most recently
used first, so if the list ever needs capping (B9, not this ticket) the tail is the stalest.

### Target snapshot

Unchanged fields, plus per-agent detail where the probe recorded it: `claude-code: available (2.1.4)`,
`codex: not installed`, or `not checked yet`. Version comes from `TargetAgent.Version` (LOOM-79). No host,
user or key ref, as today.

### Agent types in the system prompt

`router.AgentType` gets a `Description` (one line), set in `agents.ClaudeCode()` / `agents.Codex()`, and
`llmrouter.New` takes the descriptions instead of bare names. The system prompt gains:

```
Agent types:
- claude-code: Anthropic's Claude Code CLI ("claude"). A general coding agent; the default when the user doesn't name one.
- codex: OpenAI's Codex CLI ("codex"). Use when the user asks for Codex or OpenAI.
Use run_command, not an agent, when the user wants a plain shell command's output (disk space, uptime, a file listing).
```

(The default line only applies when that agent is available on the target; see the next section.)

### Go-side agent_type check (before any row)

New `router.checkAgentChoice(decision, target, message)`, called in `act` for `use_workspace` (target =
the workspace's target) and `provision_workspace` (target = `new_workspace.target_id`), before
`provisionWorkspace` or `dispatchToAgent`. It reads the recorded availability (no probe of its own):

| recorded for the chosen agent | user named that agent in the message? | another dispatchable agent recorded available on the target? | outcome |
|---|---|---|---|
| available | – | – | proceed |
| not checked | – | – | proceed; the existing pre-launch probe decides |
| not installed | yes | – | the existing LOOM-71 install offer, no rows |
| not installed | no | yes | **substitute** the available agent, log `agent substituted`, and prefix the reply "Using claude-code: codex isn't installed on jet01." |
| not installed | no | no | the existing install offer, no rows |

"Named" = the agent-type name or its binary appears as a word in the message (`codex`, `claude`,
`claude-code`), case-insensitive.

Decided 2026-10-04: **substitute** (not reject). The substitution is logged on the routing decision
(`routing decision … agent_substituted_from=codex agent_type=claude-code`). Either way the acceptance holds:
no workspace or task row is written for a decision naming an unavailable agent.

## LOOM-87: conversation affinity

### What Decide gets

Through `DispatchOptions` (filled by the router, not callers), so the `RoutingModel` interface and every
stub stay unchanged:

```go
type ConversationTurn struct{ Role, Content string }   // oldest first
type OpenTaskSnapshot struct {
    WorkspaceID, WorkspaceName, AgentType, Status string
    LastReply string  // the last assistant message tied to this task, truncated
}
// DispatchOptions gains: History []ConversationTurn; OpenTask *OpenTaskSnapshot; LastWorkspaceID string
```

- **History:** the conversation's messages before this one. Since LOOM-80 the current user message is
  already stored at submit, so it is excluded by `dispatch_id` (or, outside a job, by being the last
  message with this exact content).
- **Open task:** the conversation's agent task in `awaiting_input` (relay said `done=false`: the agent asked
  something, is waiting for confirmation, or is mid-way). Not `human_takeover`, not terminal. A reaped
  session still counts: the workspace is right and the router already relaunches there.
- **LastWorkspaceID:** the workspace of the conversation's most recent task, if no task is open. Soft
  context only ("this conversation last worked in X").

### Token budget

| block | cap | ≈ tokens |
|---|---|---|
| history | last 6 messages, each ≤ 400 runes, block ≤ 2,400 runes | ≤ 600 |
| open task | last reply ≤ 600 runes | ≤ 200 |
| workspace summaries | ≤ 240 runes each | ≈ 60 per workspace |
| agent descriptions (system prompt) | fixed | ≈ 80 |

Truncation keeps the **end** of an assistant reply (where the question usually is) and the **start** of a
user message. Today's prompt for the live instance (2 workspaces, 1 target) is roughly 1.1k tokens; with
everything full it stays under ~2.5k, well inside the primary tier's 15s budget.

Rendered:

```
Conversation so far (oldest first):
user: run df -h on jet01
assistant: / is 71% full (…)

Open task in this conversation:
  workspace: api-server (id ws-1), agent: claude-code, status: awaiting_input
  its last reply: "…I've drafted the migration. Should I also update the tests?"
If the message continues this task (an answer, a confirmation, a follow-up), choose use_workspace with workspace_id ws-1.
Set leave_open_task to true only if the message is clearly unrelated to it.
```

### Deterministic affinity rule (Go, after Decide)

The decide tool gains `leave_open_task` (boolean), offered only when there is an open task. In
`router.Dispatch`, after `Decide` and before `act`:

```
if openTask != nil && !decision.LeaveOpenTask && !(decision.Action == use_workspace && decision.WorkspaceID == openTask.WorkspaceID):
    decision = use_workspace{WorkspaceID: openTask.WorkspaceID, AgentType: openTask.AgentType}
    log "affinity override" (from_action, open task id); metric routing_decisions{action="affinity_override"}
if decision is use_workspace for the open task's workspace:
    decision.AgentType = openTask.AgentType   // same pane, whatever the model wrote
```

So "yes, go ahead" reaches the same pane even if the model answers it directly. Pending
install/clone/run offers (answered before routing) are untouched.

**Changed during implementation (2026-10-04, from eval evidence):** `run_command` and
`provision_workspace` count as an explicit opt-out, like `leave_open_task`. The first eval runs showed the
model reliably choosing `run_command "uptime"` on jet01 for "what's the uptime on jet01?" mid-task, but
setting `leave_open_task` only 1 time in 3, so the strict rule typed an unrelated question into the agent's
pane. What is overridden is what affinity exists for: the router answering a follow-up itself
(`answer_directly`) or sending it to another workspace (`use_workspace` elsewhere). This is in its own
commit so it can be reverted alone.

### Web (loomux/web, separate PR)

`api.dispatch(token, conversationId, message, workspaceHint?)` sends `workspace_hint` when the conversation
has a workspace (its latest task's `workspace_id`); `ConversationDetailPage` passes it. The server-side
affinity above doesn't depend on it; the hint helps when no task is open.

## Tests and evals

**Deterministic (TDD, `go test -race`):**
- snapshots: status/target/summary/last-used rendered; failed, archived and shell workspaces absent from the
  Decide prompt (asserted on the prompt text the stub model receives); summary truncation; recency order.
- checkAgentChoice: every row of the table; no workspace/task row written on the offer and reject paths.
- history: current message excluded; caps and truncation direction; open-task detection
  (awaiting_input yes; completed/failed/human_takeover no).
- affinity rule: answer_directly → overridden; use_workspace elsewhere → overridden; `leave_open_task` →
  kept; agent normalised; an end-to-end "agent asks, user says yes" lands in the same pane (fake executor).

**Routing evals (real model):** new `router/llmrouter/eval_test.go` behind build tag `routereval`, driven by
`testdata/route_eval.json` (fixture workspaces/targets/history/open task, message, expected action and the
fields that must match). Skipped unless `LOOMUX_ROUTER_PRIMARY_*` is set. Each case runs 3 times on the
primary tier; the report lists pass counts; the bar is 3/3 for every case. Cases:

1. jet01 has no agents, sc1 has claude-code → "start a new python project" provisions on sc1 (or, if it
   picks jet01, names an available agent).
2. "how much disk is free on jet01?" → run_command on jet01, not an agent.
3. "use codex to add a README in api-server" → use_workspace api-server, agent codex.
4. "ask claude to fix the failing test in api-server" → agent claude-code.
5. An idle workspace whose summary matches ("rate limiter in api-server") → use_workspace, not provision.
6. Open task asked "Should I also update the tests?" + "yes, go ahead" → use_workspace that workspace,
   leave_open_task false.
7. Same open task + "what's the uptime on jet01?" → run_command, leave_open_task true.
8. Open task listed three options + "do option 2" → use_workspace, same workspace.
9. History "run df -h on jet01" → result; message "now the same on sc1" → run_command `df -h` on sc1.
10. History mentions workspace X, no open task, message "and add tests for that too" → use_workspace X.

Running them needs the router model's key. I'd run them against the deployed primary tier's config if you
point me to it (or run `go test -tags routereval ./router/llmrouter/ -run Eval -v` yourself).
