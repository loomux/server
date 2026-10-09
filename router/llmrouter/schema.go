package llmrouter

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// decideToolName is the single function Decide has the model call —
// forced via tool_choice on OpenAI-compatible tiers, strict and
// prompted on Anthropic ones (LOOM-186) — so its output is always a
// structured, parseable decision rather than free text.
const decideToolName = "route_decision"

const decideSystemPrompt = `You are Loomux's routing model. Loomux dispatches chat messages to AI coding agents ` +
	`(such as Claude Code or Codex) running in workspaces on machines the user has registered, and relays ` +
	`their results back to the chat; it can also run plain shell commands on those machines. It is ` +
	`single-user — the person chatting is its one owner and operator, not a customer or a team. ` +
	`When you answer directly, speak as Loomux — concise and practical, about the user's machines, ` +
	`workspaces and agents — not as a general-purpose assistant. Only describe what Loomux can actually ` +
	`do through the actions below: never promise to remember, schedule, notify, or follow up later, ` +
	`and don't claim to have done anything you haven't — if a request needs an agent or a machine, ` +
	`route it instead of answering it yourself. ` +
	`Given an incoming chat message and a ` +
	`compact list of existing workspaces, decide what to do with it by calling the ` + decideToolName +
	` function exactly once. Choose "answer_directly" for messages that don't need any workspace ` +
	`(small talk, a question you can answer yourself) and fill in direct_answer. Choose ` +
	`"use_workspace" to dispatch the message to an existing workspace that fits it, filling in ` +
	`workspace_id (must be one of the listed workspace IDs) and agent_type. Choose ` +
	`"provision_workspace" when no existing workspace fits and a new one should be created, filling ` +
	`in new_workspace and agent_type; new_workspace.target_id must be the ID of one of the listed ` +
	`targets (the machines a workspace can be created on). Only use an agent_type from the list of ` +
	`registered agent types given to you, and only a target_id from the list of targets given to ` +
	`you — never invent either. If no targets are listed, a new workspace cannot be provisioned. ` +
	`Each target lists which agent types are installed on it: prefer an agent_type that is ` +
	`"available" on the target the work will run on, and don't choose one marked "not installed" ` +
	`there unless the user explicitly asked for that agent — Loomux will then offer to install it ` +
	`rather than run it. "not checked yet" means availability is unknown; Loomux checks before ` +
	`launching. A target marked "unusable right now" failed its last health check: don't send ` +
	`work there; if the user asked for that machine, answer directly and tell them what is wrong ` +
	`with it. A target's "policy" line says what Loomux may do there: never provision on one with ` +
	`"no new workspaces", run_command on one with "no shell commands", or choose an agent_type its ` +
	`"only" list leaves out — Loomux refuses those regardless; prefer another target, or answer ` +
	`directly and say why. A new workspace is described only by its name, kind (empty, git_clone or ` +
	`existing_dir) and, for git_clone, the git_remote URL: Loomux creates it under the target's ` +
	`workspace root itself, so you never supply a path or any provisioning command. ` +
	`Choose "run_command" when the user asks to run a plain shell command on a machine ` +
	`(no AI agent needed): set target_id to one of the listed targets and command to the command ` +
	`exactly as the user wrote it, character for character — never invent, expand, combine or ` +
	`"fix" a command. Only if the user describes what they want without giving the command may you ` +
	`propose one in command; Loomux then shows it to the user and runs it only after they confirm. ` +
	`You cannot see any machine's live state: never answer a question about a machine's disk, ` +
	`memory, uptime, load, processes, files or services yourself — choose run_command with the ` +
	`command that answers it (if the user didn't give one, propose it; Loomux asks before running a ` +
	`command you proposed). Use run_command rather than an agent whenever a plain shell command's ` +
	`output answers the request; an agent is for work that needs reading, writing or reasoning ` +
	`about code. ` +
	`Each workspace lists its status, the target it is on, when it was last used and what was last ` +
	`done there: prefer an idle or active workspace whose description or recent work fits the ` +
	`message over provisioning a new one; a provisioning workspace isn't ready yet.`

// systemPrompt is decideSystemPrompt plus the registered agent types,
// each with its description (LOOM-88).
func (m *Model) systemPrompt() string {
	if len(m.agentTypes) == 0 {
		return decideSystemPrompt
	}
	var b strings.Builder
	b.WriteString(decideSystemPrompt)
	b.WriteString("\n\nAgent types:")
	names := append([]string(nil), m.agentTypes...)
	sort.Strings(names)
	for _, name := range names {
		if d := m.agentDescriptions[name]; d != "" {
			fmt.Fprintf(&b, "\n- %s: %s", name, d)
		} else {
			fmt.Fprintf(&b, "\n- %s", name)
		}
	}
	return b.String()
}

// relayToolName is the single function Relay has the model call (as
// decideToolName), so it always returns both the condensed reply and
// the done/continues signal as structured, parseable output.
const relayToolName = "condense_output"

const relaySystemPrompt = `You are Loomux's relay model. You are given the raw captured output of an ` +
	`agent's terminal session, captured right after it went quiet (finished a turn). Call the ` +
	relayToolName + ` function exactly once with two things: reply — exactly one plain-text response, ` +
	`no markdown headers, no preamble, that condenses what the agent did, what changed, and what (if ` +
	`anything) is needed next; this single string is shown directly to the user as the chat reply AND ` +
	`stored as the workspace's rolling summary, so it must stand alone as both — concise, not a ` +
	`verbatim transcript. And done — true if the task itself is fully finished and no follow-up is ` +
	`expected (the requested work is complete, or the agent gave a final answer), false if the ` +
	`conversation is expected to continue (the agent is asking a clarifying question, waiting on ` +
	`confirmation, or mid-way through a multi-step task) — false keeps the same session open so the ` +
	`next message is typed into it directly rather than starting a fresh one. If the agent's last ` +
	`message asks the user anything or says it is waiting for them, done is false. If it says work is ` +
	`still running (a command left in the background, a build or job it will report on later), done ` +
	`is false: its report comes into the same session. ` +
	`With the output you are given the user's message that started the turn and the workspace's ` +
	`summary from before it. The reply answers that message from what the output shows, never from ` +
	`the message alone: if the output doesn't show the asked-for work done, say what it does show. ` +
	`Don't repeat the message back. Your reply replaces the summary, so if the previous summary holds ` +
	`something the next turn still needs that this output doesn't restate, keep it in one short clause.`

// decideTool builds the tool/function-call schema for Decide.
// Flat rather than a conditional schema keyed on action — conditional
// (if/then) JSON Schema support across OpenAI-compatible providers is
// unverified, so cross-field consistency (e.g. use_workspace with an empty
// workspace_id) is validated in Go after parsing instead. agentType,
// workspace_id and new_workspace.target_id are enum-constrained to the
// caller-supplied valid sets when non-empty — a cheap, high-value
// correctness win.
//
// leave_open_task (LOOM-87) is offered only when the conversation has a
// task waiting on the user (openTask).
func decideTool(agentTypes, workspaceIDs, targetIDs []string, openTask bool) toolSpec {
	// Provisioning and direct commands both need a target: with none
	// registered they aren't offered at all (LOOM-68).
	actions := []string{"answer_directly", "use_workspace"}
	if len(targetIDs) > 0 {
		actions = append(actions, "provision_workspace", "run_command")
	}
	properties := map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        actions,
			"description": "What to do with the incoming message. answer_directly is never for a question about a machine's live state (disk, memory, uptime, processes, files): use run_command for those.",
		},
		"direct_answer": map[string]any{
			"type":        "string",
			"description": "Set when action == answer_directly: the reply to send directly to the user.",
		},
		"workspace_id": enumStringProperty(
			"Set when action == use_workspace: the ID of the workspace to dispatch to.", workspaceIDs),
		"agent_type": enumStringProperty(
			"Set when action == use_workspace or provision_workspace: which registered agent type to dispatch to.", agentTypes),
		"target_id": enumStringProperty(
			"Set when action == run_command: the ID of the registered target to run the command on.", targetIDs),
		"command": map[string]any{
			"type": "string",
			"description": "Set when action == run_command: the shell command, exactly as the user wrote it. " +
				"Never invent or alter a command.",
		},
		"new_workspace": map[string]any{
			"type":        "object",
			"description": "Set when action == provision_workspace: the new workspace to create.",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"pattern":     router.WorkspaceNamePattern,
					"description": "Workspace name, also its directory under the target's workspace root: lowercase letters, digits, dashes and underscores, starting with a letter or digit.",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"empty", "git_clone", "existing_dir"},
					"description": "empty: a new empty directory; git_clone: clone git_remote; existing_dir: a directory that already exists under the workspace root.",
				},
				"target_id": enumStringProperty(
					"The ID of the registered target to create the workspace on.", targetIDs),
				"git_remote": map[string]any{
					"type":        "string",
					"description": "Only for kind git_clone: an https:// or ssh:// repository URL, or scp-style git@server:owner/repo.",
				},
				"description": map[string]any{"type": "string"},
				"tags": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
		},
	}

	if openTask {
		properties["leave_open_task"] = map[string]any{
			"type": "boolean",
			"description": "Only while the conversation has an open task: true if this message is clearly unrelated to it " +
				"(a new topic, not an answer, confirmation or follow-up). Otherwise leave it false.",
		}
	}

	return toolSpec{
		name:        decideToolName,
		description: "Record the routing decision for this message.",
		schema: map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   []string{"action"},
		},
	}
}

// relayTool builds the tool/function-call schema for Relay:
// the condensed reply text plus the done/continues signal (design spec
// §3 step 3), so that signal is always structured rather than parsed
// out of free text.
func relayTool() toolSpec {
	return toolSpec{
		name:        relayToolName,
		description: "Record the condensed reply and whether the task is fully finished.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reply": map[string]any{
					"type":        "string",
					"description": "The condensed, chat-appropriate reply — also stored as the workspace's rolling summary.",
				},
				"done": map[string]any{
					"type":        "boolean",
					"description": "true if the task is fully finished (tear the session down); false if the conversation should stay open for a follow-up in the same session.",
				},
			},
			"required": []string{"reply", "done"},
		},
	}
}

func enumStringProperty(description string, values []string) map[string]any {
	p := map[string]any{
		"type":        "string",
		"description": description,
	}
	if len(values) > 0 {
		p["enum"] = values
	}
	return p
}

// nowFunc is the clock relative times in the prompt are measured
// against; a var so tests can pin it.
var nowFunc = time.Now

// renderPolicy describes a target's policy (LOOM-89), "" for the default
// allow-everything one.
func renderPolicy(p registry.TargetPolicy) string {
	var parts []string
	if p.Purpose == registry.TargetPurposeWork {
		parts = append(parts, "work machine")
	}
	if p.NoProvision {
		parts = append(parts, "no new workspaces")
	}
	if p.NoShell {
		parts = append(parts, "no shell commands")
	}
	if len(p.AllowedAgentTypes) > 0 {
		parts = append(parts, "only "+strings.Join(p.AllowedAgentTypes, ", "))
	}
	if p.RequireConfirmation {
		parts = append(parts, "asks the user to confirm new work")
	}
	return strings.Join(parts, "; ")
}

// renderAgents lists a target's recorded agent availability (LOOM-71)
// in a stable order, with each available agent's probed version
// (LOOM-88), e.g. "claude-code: available (2.1.4), codex: not installed".
func renderAgents(agents map[string]bool, versions map[string]string) string {
	if len(agents) == 0 {
		return "not checked yet"
	}
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		state := "not installed"
		if agents[name] {
			state = "available"
			if v := versions[name]; v != "" {
				state += " (" + v + ")"
			}
		}
		parts[i] = name + ": " + state
	}
	return strings.Join(parts, ", ")
}

// relativeAge renders how long before now t was, coarsely.
func relativeAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// renderWorkspace writes one workspace entry: what it is, what state it
// is in and what was last done there (LOOM-88).
func renderWorkspace(b *strings.Builder, ws router.WorkspaceSnapshot, now time.Time) {
	fmt.Fprintf(b, "- id: %s\n  name: %s\n", ws.ID, ws.Name)
	state := ws.Status
	if state == "" {
		state = "unknown"
	}
	if ws.TargetName != "" {
		state += ", on " + ws.TargetName
	}
	if ws.LastUsed != nil {
		state += ", last used " + relativeAge(now, *ws.LastUsed)
	} else {
		state += ", never used"
	}
	fmt.Fprintf(b, "  status: %s\n  description: %s\n  tags: %s\n",
		state, ws.Description, strings.Join(ws.Tags, ", "))
	if ws.Summary != "" {
		fmt.Fprintf(b, "  recent: %s\n", ws.Summary)
	}
}

// renderConversation writes the conversation's context (LOOM-87) ahead
// of the message: its recent turns, then the task waiting on the user —
// with what to do about it — or else the workspace it last worked in.
func renderConversation(b *strings.Builder, o router.DispatchOptions) {
	if len(o.History) > 0 {
		b.WriteString("Conversation so far (oldest first):\n")
		for _, turn := range o.History {
			fmt.Fprintf(b, "%s: %s\n", turn.Role, strings.ReplaceAll(turn.Content, "\n", " "))
		}
		b.WriteString("\n")
	}
	if t := o.OpenTask; t != nil {
		fmt.Fprintf(b, "Open task in this conversation:\n  workspace: %s (id %s), agent: %s, status: %s\n",
			t.WorkspaceName, t.WorkspaceID, t.AgentType, t.Status)
		if t.LastReply != "" {
			fmt.Fprintf(b, "  its last reply: %q\n", strings.ReplaceAll(t.LastReply, "\n", " "))
		}
		fmt.Fprintf(b, "If the message continues this task (an answer, a confirmation, a choice, a follow-up), "+
			"choose use_workspace with workspace_id %s. If the message is clearly unrelated to it (a new "+
			"topic or a different machine), handle it as you would otherwise and set leave_open_task to true: "+
			"an answer_directly, or a use_workspace for another workspace, without leave_open_task is sent to this task instead.\n\n",
			t.WorkspaceID)
	} else if o.LastWorkspaceID != "" {
		fmt.Fprintf(b, "This conversation last worked in workspace %s (id %s).\n\n", o.LastWorkspaceName, o.LastWorkspaceID)
	}
}

// decideUserPrompt renders the message, compact workspace registry
// (design spec §6, enriched with status/target/recency in LOOM-88) and
// registered targets (LOOM-64: id, name and kind only — plus each one's
// recorded agent availability, LOOM-71) as the user turn.
// o.WorkspaceHint (LOOM-46), when non-empty, is appended as advisory
// context — the client's suggested workspace_id, which the model may
// follow or disregard; it's never substituted for the model's own
// use_workspace decision.
func decideUserPrompt(message string, workspaces []router.WorkspaceSnapshot, targets []router.TargetSnapshot, o router.DispatchOptions) string {
	now := nowFunc()
	var b strings.Builder
	renderConversation(&b, o)
	b.WriteString("Message:\n")
	b.WriteString(message)
	b.WriteString("\n\nWorkspaces:\n")
	if len(workspaces) == 0 {
		b.WriteString("(none)\n")
	}
	for _, ws := range workspaces {
		renderWorkspace(&b, ws, now)
	}
	b.WriteString("\nTargets:\n")
	if len(targets) == 0 {
		b.WriteString("(none)\n")
		b.WriteString("\nNo targets are registered, so no workspace can be provisioned and no command run. " +
			"If the message needs work done on a machine, answer directly and tell the user to register a " +
			"target first, on the Targets page or with POST /api/v1/targets.\n")
	}
	for _, t := range targets {
		fmt.Fprintf(&b, "- id: %s\n  name: %s\n  kind: %s\n  agents: %s\n", t.ID, t.Name, t.Kind, renderAgents(t.Agents, t.AgentVersions))
		if t.CreatedBy != "" {
			note := "a machine Loomux created with the " + t.CreatedBy + " plugin; only its own workspaces are there"
			if t.Ephemeral {
				note += "; ephemeral: its files go when it is destroyed"
			}
			fmt.Fprintf(&b, "  created by plugin: %s\n", note)
			if t.MachineStatus == "stopped" {
				b.WriteString("  stopped: it will be started when used\n")
			}
		}
		if t.Problem != "" {
			fmt.Fprintf(&b, "  unusable right now: %s\n", t.Problem)
		}
		if p := renderPolicy(t.Policy); p != "" {
			fmt.Fprintf(&b, "  policy: %s\n", p)
		}
	}
	if o.WorkspaceHint != "" {
		fmt.Fprintf(&b, "\nClient hint: the caller suggests this message likely belongs to workspace_id %q. "+
			"Treat this as advisory only — use your own judgement and the message content to decide.\n", o.WorkspaceHint)
	}
	return b.String()
}
