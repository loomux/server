package llmrouter

import (
	"fmt"
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
	`in new_workspace and agent_type. Only use an agent_type from the list of registered agent types ` +
	`given to you — never invent one.`

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
// workspace_id) is validated in Go after parsing instead. agentType and
// workspace_id are enum-constrained to the caller-supplied valid sets when
// non-empty — a cheap, high-value correctness win.
func buildDecideTool(agentTypes, workspaceIDs []string) openai.ChatCompletionToolUnionParam {
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
				"name":              map[string]any{"type": "string"},
				"path":              map[string]any{"type": "string"},
				"target_id":         map[string]any{"type": "string"},
				"git_remote":        map[string]any{"type": "string"},
				"description":       map[string]any{"type": "string"},
				"provision_command": map[string]any{"type": "string"},
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

// decideUserPrompt renders the message and compact workspace registry
// (design spec §6: tags/description/capabilities, not full history) as the
// user turn.
func decideUserPrompt(message string, workspaces []router.WorkspaceSnapshot) string {
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
	return b.String()
}
