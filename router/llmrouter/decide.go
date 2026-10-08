package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Loomux/server/internal/metrics"
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
	TargetID     string                   `json:"target_id"`
	Command      string                   `json:"command"`
	// LeaveOpenTask (LOOM-87) is only offered while a task is open.
	LeaveOpenTask bool `json:"leave_open_task"`
}

// decideArgumentsWorkspace is structured data only (LOOM-90): no command
// and no path — Loomux builds provisioning itself from these fields.
type decideArgumentsWorkspace struct {
	Name        string   `json:"name"`
	TargetID    string   `json:"target_id"`
	Kind        string   `json:"kind"`
	GitRemote   string   `json:"git_remote"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Decide implements router.RoutingModel. It tries the primary tier first,
// escalating to the configured escalation tier (if any) when the primary's
// output can't be used (transport/rate-limit failure, or an unparseable/
// invalid tool call) or the primary is unavailable. opts' WithWorkspaceHint
// (LOOM-46), if set, is folded into the user prompt as advisory context —
// it never bypasses the model's own workspace_id validation against
// workspaces. targets (LOOM-64) are listed in the prompt and bound
// new_workspace.target_id the same way workspaces bound workspace_id.
func (m *Model) Decide(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot, targets []router.TargetSnapshot, opts ...router.DispatchOption) (router.Decision, error) {
	var o router.DispatchOptions
	for _, opt := range opts {
		opt(&o)
	}

	var err error
	if m.skipPrimary() {
		err = errPrimarySkipped
	} else {
		var dec router.Decision
		dec, err = m.decideWith(ctx, "primary", m.cfg.Primary, m.primaryTimeout, message, workspaces, targets, o)
		m.recordPrimary(ctx, err)
		if err == nil {
			return dec, nil
		}
	}
	if m.cfg.Escalation == nil {
		m.metrics.RecordRouterCall(metrics.RouterOpDecide, "primary", metrics.OutcomeFailure, m.primaryTimeout)
		return router.Decision{}, fmt.Errorf("llmrouter: decide: primary model: %w", err)
	}

	m.metrics.RecordRouterEscalation(metrics.RouterOpDecide)
	dec, err2 := m.decideWith(ctx, "escalation", *m.cfg.Escalation, m.escalationTimeout, message, workspaces, targets, o)
	if err2 != nil {
		m.metrics.RecordRouterCall(metrics.RouterOpDecide, "escalation", metrics.OutcomeFailure, m.escalationTimeout)
		return router.Decision{}, fmt.Errorf(
			"llmrouter: decide: primary model failed (%v); escalation model also failed: %w", err, err2)
	}
	return dec, nil
}

func (m *Model) decideWith(ctx context.Context, tierName string, tier Tier, timeout time.Duration, message string, workspaces []router.WorkspaceSnapshot, targets []router.TargetSnapshot, o router.DispatchOptions) (router.Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workspaceIDs := make([]string, len(workspaces))
	for i, ws := range workspaces {
		workspaceIDs[i] = ws.ID
	}
	targetIDs := make([]string, len(targets))
	for i, t := range targets {
		targetIDs[i] = t.ID
	}

	ex := newExchange(tier, m.systemPrompt(), decideUserPrompt(message, workspaces, targets, o),
		decideTool(m.agentTypes, workspaceIDs, targetIDs, o.OpenTask != nil))
	// A tool call that can't be used gets one more try on the same tier,
	// told what was wrong (LOOM-107): a hallucinated id is usually fixed
	// by saying so, and cheaper than escalating. Both attempts share the
	// tier's timeout.
	var decision router.Decision
	err := m.converse(ctx, metrics.RouterOpDecide, tierName, ex, decideToolName, correctionPrompt, func(raw json.RawMessage) error {
		var args decideArguments
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unparseable tool call arguments: %w", err)
		}
		d, problem := m.validateDecision(args, workspaceIDs, targetIDs)
		if problem != nil {
			return problem
		}
		d.LeaveOpenTask = o.OpenTask != nil && args.LeaveOpenTask
		d.Model, d.Tier = tier.Model, tierName
		decision = d
		return nil
	})
	if err != nil {
		return router.Decision{}, err
	}
	return decision, nil
}

// correctionPrompt tells the model why its route_decision call was
// refused, for the corrective retry.
func correctionPrompt(problem error) string {
	return "Your previous " + decideToolName + " call was rejected: " + problem.Error() +
		". Call " + decideToolName + " again with a valid decision: use only the ids, agent types and values listed above."
}

// validateDecision converts parsed tool-call arguments into a
// router.Decision, rejecting anything Router.Dispatch couldn't act on
// safely: an unknown action, an agent_type outside the caller-supplied
// set, a use_workspace decision naming a workspace_id that wasn't in
// this call's workspace list, or a provision_workspace decision naming a
// target_id that wasn't in this call's target list (LOOM-64), or a
// run_command (LOOM-72) with an unknown target_id or no command.
func (m *Model) validateDecision(args decideArguments, workspaceIDs, targetIDs []string) (router.Decision, error) {
	action := router.DecisionAction(args.Action)
	// With no targets registered, provisioning and direct commands aren't
	// offered (LOOM-68). A model that picks one anyway wanted a machine
	// that doesn't exist: that's answered — register a target first — not
	// treated as a model failure to escalate or surface as an error.
	if len(targetIDs) == 0 && (action == router.ActionProvisionWorkspace || action == router.ActionRunCommand) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: router.NoTargetsReply}, nil
	}
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
		if args.NewWorkspace.TargetID == "" || !contains(targetIDs, args.NewWorkspace.TargetID) {
			return router.Decision{}, fmt.Errorf("provision_workspace with unknown target_id %q", args.NewWorkspace.TargetID)
		}
		spec := router.ProvisionSpec{
			Name:        args.NewWorkspace.Name,
			TargetID:    args.NewWorkspace.TargetID,
			Kind:        router.ProvisionKind(args.NewWorkspace.Kind),
			GitRemote:   args.NewWorkspace.GitRemote,
			Description: args.NewWorkspace.Description,
			Tags:        args.NewWorkspace.Tags,
		}
		if err := spec.Validate(); err != nil {
			return router.Decision{}, fmt.Errorf("provision_workspace: %w", err)
		}
		return router.Decision{Action: action, AgentType: args.AgentType, NewWorkspace: spec}, nil

	case router.ActionRunCommand:
		if args.TargetID == "" || !contains(targetIDs, args.TargetID) {
			return router.Decision{}, fmt.Errorf("run_command with unknown target_id %q", args.TargetID)
		}
		if strings.TrimSpace(args.Command) == "" {
			return router.Decision{}, fmt.Errorf("run_command with empty command")
		}
		return router.Decision{Action: action, TargetID: args.TargetID, Command: args.Command}, nil

	default:
		return router.Decision{}, fmt.Errorf("unknown action %q", args.Action)
	}
}
