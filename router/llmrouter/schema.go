package llmrouter

import (
	"fmt"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Loomux/server/router"
)

// decideToolName is the single function Decide forces the model to call
// via tool_choice, so its output is always a structured, parseable
// decision rather than free text.
const decideToolName = "route_decision"

const decideSystemPrompt = `You are Loomux's routing model. Given an incoming chat message and a ` +
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
	`launching. A new workspace is described only by its name, kind (empty, git_clone or ` +
	`existing_dir) and, for git_clone, the git_remote URL: Loomux creates it under the target's ` +
	`workspace root itself, so you never supply a path or any provisioning command.`

// relayToolName is the single function Relay forces the model to call
// via tool_choice, so it always returns both the condensed reply and
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
	`next message is typed into it directly rather than starting a fresh one.`

// buildDecideTool builds the forced tool/function-call schema for Decide.
// Flat rather than a conditional schema keyed on action — conditional
// (if/then) JSON Schema support across OpenAI-compatible providers is
// unverified, so cross-field consistency (e.g. use_workspace with an empty
// workspace_id) is validated in Go after parsing instead. agentType,
// workspace_id and new_workspace.target_id are enum-constrained to the
// caller-supplied valid sets when non-empty — a cheap, high-value
// correctness win.
func buildDecideTool(agentTypes, workspaceIDs, targetIDs []string) openai.ChatCompletionToolUnionParam {
	properties := map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []string{"answer_directly", "use_workspace", "provision_workspace"},
			"description": "What to do with the incoming message.",
		},
		"direct_answer": map[string]any{
			"type":        "string",
			"description": "Set when action == answer_directly: the reply to send directly to the user.",
		},
		"workspace_id": enumStringProperty(
			"Set when action == use_workspace: the ID of the workspace to dispatch to.", workspaceIDs),
		"agent_type": enumStringProperty(
			"Set when action == use_workspace or provision_workspace: which registered agent type to dispatch to.", agentTypes),
		"new_workspace": map[string]any{
			"type":        "object",
			"description": "Set when action == provision_workspace: the new workspace to create.",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"pattern":     "^[a-z0-9][a-z0-9-]{0,62}$",
					"description": "Workspace name, also its directory under the target's workspace root: lowercase letters, digits and dashes.",
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

	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        decideToolName,
		Description: openai.String("Record the routing decision for this message."),
		Parameters: shared.FunctionParameters{
			"type":       "object",
			"properties": properties,
			"required":   []string{"action"},
		},
	})
}

// buildRelayTool builds the forced tool/function-call schema for Relay:
// the condensed reply text plus the done/continues signal (design spec
// §3 step 3), so that signal is always structured rather than parsed
// out of free text.
func buildRelayTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        relayToolName,
		Description: openai.String("Record the condensed reply and whether the task is fully finished."),
		Parameters: shared.FunctionParameters{
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
	})
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

// renderAgents lists a target's recorded agent availability (LOOM-71) in
// a stable order, e.g. "claude-code: available, codex: not installed".
func renderAgents(agents map[string]bool) string {
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
		}
		parts[i] = name + ": " + state
	}
	return strings.Join(parts, ", ")
}

// decideUserPrompt renders the message, compact workspace registry
// (design spec §6: tags/description/capabilities, not full history) and
// registered targets (LOOM-64: id, name and kind only — plus each one's
// recorded agent availability, LOOM-71) as the user turn.
// workspaceHint (LOOM-46), when non-empty, is appended as advisory
// context — the client's suggested workspace_id, which the model may
// follow or disregard; it's never substituted for the model's own
// use_workspace decision.
func decideUserPrompt(message string, workspaces []router.WorkspaceSnapshot, targets []router.TargetSnapshot, workspaceHint string) string {
	var b strings.Builder
	b.WriteString("Message:\n")
	b.WriteString(message)
	b.WriteString("\n\nWorkspaces:\n")
	if len(workspaces) == 0 {
		b.WriteString("(none)\n")
	}
	for _, ws := range workspaces {
		fmt.Fprintf(&b, "- id: %s\n  name: %s\n  description: %s\n  tags: %s\n  capabilities: %s\n",
			ws.ID, ws.Name, ws.Description, strings.Join(ws.Tags, ", "), strings.Join(ws.Capabilities, ", "))
	}
	b.WriteString("\nTargets:\n")
	if len(targets) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range targets {
		fmt.Fprintf(&b, "- id: %s\n  name: %s\n  kind: %s\n  agents: %s\n", t.ID, t.Name, t.Kind, renderAgents(t.Agents))
	}
	if workspaceHint != "" {
		fmt.Fprintf(&b, "\nClient hint: the caller suggests this message likely belongs to workspace_id %q. "+
			"Treat this as advisory only — use your own judgement and the message content to decide.\n", workspaceHint)
	}
	return b.String()
}
