package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
)

// policyTarget is the target decision would act on, or nil if it acts
// on none (an answer) or names one that doesn't exist — act reports
// that itself.
func (r *Router) policyTarget(ctx context.Context, decision Decision) (*registry.Target, error) {
	var targetID string
	switch decision.Action {
	case ActionProvisionWorkspace:
		targetID = decision.NewWorkspace.TargetID
	case ActionRunCommand:
		targetID = decision.TargetID
	case ActionUseWorkspace:
		ws, err := r.store.GetWorkspace(ctx, decision.WorkspaceID)
		if errors.Is(err, registry.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		targetID = ws.TargetID
	default:
		return nil, nil
	}
	target, err := r.store.GetTarget(ctx, targetID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil
	}
	return target, err
}

// policyRefusal is why target's policy forbids decision (LOOM-89), as a
// reply to the user, or "" if it doesn't.
func policyRefusal(target *registry.Target, decision Decision) string {
	p := target.Policy
	switch decision.Action {
	case ActionProvisionWorkspace:
		if p.NoProvision {
			return fmt.Sprintf("I can't do that: %s's policy doesn't allow new workspaces there. "+
				"Use one of its existing workspaces, another target, or change the target's policy on the Targets page.", target.Name)
		}
	case ActionRunCommand:
		if p.NoShell {
			return fmt.Sprintf("I can't do that: %s's policy doesn't allow plain shell commands there. "+
				"Change the target's policy on the Targets page if it should.", target.Name)
		}
	}
	if (decision.Action == ActionProvisionWorkspace || decision.Action == ActionUseWorkspace) &&
		decision.AgentType != "" && !p.AllowsAgent(decision.AgentType) {
		return fmt.Sprintf("I can't do that: %s's policy doesn't allow %s there; it allows only %s.",
			target.Name, decision.AgentType, strings.Join(p.AllowedAgentTypes, ", "))
	}
	return ""
}

// enforcePolicy applies the policy of the target decision would act on
// (LOOM-89), after routing and before anything runs: a forbidden decision
// ends the turn with a refusal, and new work on a require-confirmation
// target ends it with the plan and an offer to carry it out on "yes".
// continuing is set when decision only carries on the conversation's open
// task in its own workspace, which was confirmed when it started.
// handled reports whether the turn ended here.
func (r *Router) enforcePolicy(ctx context.Context, log *slog.Logger, conversationID, message string, decision Decision,
	continuing bool, start time.Time) (reply string, handled bool, err error) {
	target, err := r.policyTarget(ctx, decision)
	if err != nil {
		return "", true, fmt.Errorf("router: dispatch: policy: %w", err)
	}
	if target == nil {
		return "", false, nil
	}
	if refusal := policyRefusal(target, decision); refusal != "" {
		log.Info("decision refused by target policy", "target_id", target.ID, "decided_action", string(decision.Action),
			"agent_type", decision.AgentType)
		reply, err := r.finishTurn(ctx, log, conversationID, message, "", refusal, "policy_refused", start)
		return reply, true, err
	}
	if !target.Policy.RequireConfirmation || continuing {
		return "", false, nil
	}
	// A command not given verbatim is shown back for a "yes" by
	// runCommand itself: one confirmation is enough.
	if decision.Action == ActionRunCommand && !orderedVerbatim(message, decision.Command, target) {
		return "", false, nil
	}
	plan, err := r.describePlan(ctx, target, decision)
	if err != nil {
		return "", true, fmt.Errorf("router: dispatch: policy: %w", err)
	}
	d := decision
	r.pending.put(conversationID, pendingInstall{
		kind: pendingPolicyConfirm, targetID: target.ID, agentType: decision.AgentType,
		decision: &d, message: message, expires: time.Now().Add(pendingTTL),
	})
	purpose := ""
	if target.Policy.Purpose == registry.TargetPurposeWork {
		purpose = " (a work machine)"
	}
	offer := fmt.Sprintf("%s%s asks for confirmation before new work starts there. I would %s.\n\n"+
		`Reply "yes" to go ahead. Any other reply cancels.`, target.Name, purpose, plan)
	log.Info("decision awaits policy confirmation", "target_id", target.ID, "decided_action", string(decision.Action))
	reply, err = r.finishTurn(ctx, log, conversationID, message, "", offer, "policy_confirmation_requested", start)
	return reply, true, err
}

// describePlan says what decision will do on target, for a confirmation.
func (r *Router) describePlan(ctx context.Context, target *registry.Target, decision Decision) (string, error) {
	switch decision.Action {
	case ActionProvisionWorkspace:
		s := decision.NewWorkspace
		what := "an empty workspace"
		switch s.Kind {
		case ProvisionGitClone:
			what = "a workspace cloned from " + s.GitRemote
		case ProvisionExistingDir:
			what = "a workspace from the existing directory"
		}
		return fmt.Sprintf("create %s named %s at %s on %s, and start %s there",
			what, s.Name, workspaceDisplayPath(target, s.Name), target.Name, decision.AgentType), nil
	case ActionUseWorkspace:
		ws, err := r.store.GetWorkspace(ctx, decision.WorkspaceID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("start %s in workspace %s on %s", decision.AgentType, ws.Name, target.Name), nil
	case ActionRunCommand:
		return fmt.Sprintf("run this on %s:\n\n    %s", target.Name, strings.TrimSpace(decision.Command)), nil
	}
	return "", fmt.Errorf("no plan for action %q", decision.Action)
}

// confirmPolicy carries out a plan its target's policy made wait for a
// "yes" (LOOM-89), refusing it if the policy has since been tightened.
func (r *Router) confirmPolicy(ctx context.Context, log *slog.Logger, conversationID string, p pendingInstall,
	start time.Time, m *dispatchMetrics) (string, error) {
	decision := *p.decision
	log.Info("policy confirmation given", "target_id", p.targetID, "decided_action", string(decision.Action))
	m.action = string(decision.Action)
	if target, err := r.policyTarget(ctx, decision); err != nil {
		return "", fmt.Errorf("router: dispatch: policy: %w", err)
	} else if target != nil {
		if refusal := policyRefusal(target, decision); refusal != "" {
			return r.finishTurn(ctx, log, conversationID, p.message, "", refusal, "policy_refused", start)
		}
	}
	return r.act(ctx, log, conversationID, p.message, decision, false, start, m)
}
