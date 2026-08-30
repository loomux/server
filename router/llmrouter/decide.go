package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/Loomux/server/router"
)

// decideArguments mirrors router.Decision + router.ProvisionSpec, flattened
// into the shape route_decision's tool schema asks the model to fill in.
type decideArguments struct {
	Action       string                   `json:"action"`
	DirectAnswer string                   `json:"direct_answer"`
	WorkspaceID  string                   `json:"workspace_id"`
	AgentType    string                   `json:"agent_type"`
	NewWorkspace decideArgumentsWorkspace `json:"new_workspace"`
}

type decideArgumentsWorkspace struct {
	Name             string   `json:"name"`
	Path             string   `json:"path"`
	TargetID         string   `json:"target_id"`
	GitRemote        string   `json:"git_remote"`
	Description      string   `json:"description"`
	Tags             []string `json:"tags"`
	ProvisionCommand string   `json:"provision_command"`
}

// Decide implements router.RoutingModel. It tries the primary tier first,
// escalating to the configured escalation tier (if any) when the primary's
// output can't be used (transport/rate-limit failure, or an unparseable/
// invalid tool call) or the primary is unavailable.
func (m *Model) Decide(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
	dec, err := m.decideWith(ctx, m.cfg.Primary, m.primaryTimeout, message, workspaces)
	if err == nil {
		return dec, nil
	}
	if m.cfg.Escalation == nil {
		return router.Decision{}, fmt.Errorf("llmrouter: decide: primary model: %w", err)
	}

	dec, err2 := m.decideWith(ctx, *m.cfg.Escalation, m.escalationTimeout, message, workspaces)
	if err2 != nil {
		return router.Decision{}, fmt.Errorf(
			"llmrouter: decide: primary model failed (%v); escalation model also failed: %w", err, err2)
	}
	return dec, nil
}

func (m *Model) decideWith(ctx context.Context, tier Tier, timeout time.Duration, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workspaceIDs := make([]string, len(workspaces))
	for i, ws := range workspaces {
		workspaceIDs[i] = ws.ID
	}

	client := buildClient(tier)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: tier.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(decideSystemPrompt),
			openai.UserMessage(decideUserPrompt(message, workspaces)),
		},
		Tools: []openai.ChatCompletionToolUnionParam{buildDecideTool(m.agentTypes, workspaceIDs)},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
			Name: decideToolName,
		}),
	})
	if err != nil {
		return router.Decision{}, fmt.Errorf("call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return router.Decision{}, fmt.Errorf("no choices returned")
	}

	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		return router.Decision{}, fmt.Errorf("model did not call %s", decideToolName)
	}

	var args decideArguments
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		return router.Decision{}, fmt.Errorf("unparseable tool call arguments: %w", err)
	}

	return m.validateDecision(args, workspaceIDs)
}

// validateDecision converts parsed tool-call arguments into a
// router.Decision, rejecting anything Router.Dispatch couldn't act on
// safely: an unknown action, an agent_type outside the caller-supplied
// set, or a use_workspace decision naming a workspace_id that wasn't in
// this call's workspace list.
func (m *Model) validateDecision(args decideArguments, workspaceIDs []string) (router.Decision, error) {
	action := router.DecisionAction(args.Action)
	switch action {
	case router.ActionAnswerDirectly:
		if strings.TrimSpace(args.DirectAnswer) == "" {
			return router.Decision{}, fmt.Errorf("answer_directly with empty direct_answer")
		}
		return router.Decision{Action: action, DirectAnswer: args.DirectAnswer}, nil

	case router.ActionUseWorkspace:
		if args.WorkspaceID == "" || !contains(workspaceIDs, args.WorkspaceID) {
			return router.Decision{}, fmt.Errorf("use_workspace with unknown workspace_id %q", args.WorkspaceID)
		}
		if !m.isValidAgentType(args.AgentType) {
			return router.Decision{}, fmt.Errorf("unknown agent_type %q", args.AgentType)
		}
		return router.Decision{Action: action, WorkspaceID: args.WorkspaceID, AgentType: args.AgentType}, nil

	case router.ActionProvisionWorkspace:
		if !m.isValidAgentType(args.AgentType) {
			return router.Decision{}, fmt.Errorf("unknown agent_type %q", args.AgentType)
		}
		return router.Decision{
			Action:    action,
			AgentType: args.AgentType,
			NewWorkspace: router.ProvisionSpec{
				Name:             args.NewWorkspace.Name,
				Path:             args.NewWorkspace.Path,
				TargetID:         args.NewWorkspace.TargetID,
				GitRemote:        args.NewWorkspace.GitRemote,
				Description:      args.NewWorkspace.Description,
				Tags:             args.NewWorkspace.Tags,
				ProvisionCommand: args.NewWorkspace.ProvisionCommand,
			},
		}, nil

	default:
		return router.Decision{}, fmt.Errorf("unknown action %q", args.Action)
	}
}
