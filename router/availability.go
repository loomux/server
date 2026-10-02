package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
)

// What an agent CLI probe prints. A sentinel rather than the exit status:
// TargetExecutor.RunOnce reports a non-zero exit as an error, which would
// leave "not installed" indistinguishable from "couldn't run the probe".
const (
	agentProbePresent = "loomux-agent-present"
	agentProbeAbsent  = "loomux-agent-absent"
)

// AgentUnavailableError is returned when an agent-type's CLI isn't
// installed on the target a dispatch needs it on (LOOM-71). Dispatch
// turns it into a reply — an install offer when the agent-type has a
// recipe — rather than a failure.
type AgentUnavailableError struct {
	AgentType  string
	TargetID   string
	TargetName string
}

func (e *AgentUnavailableError) Error() string {
	return fmt.Sprintf("agent %q is not installed on target %q", e.AgentType, e.TargetName)
}

// agentProbeCommand checks for binary through POSIX sh whatever the
// target's login shell is, printing one of the probe sentinels.
func agentProbeCommand(binary string) string {
	inner := "command -v " + shellQuote(binary) + " >/dev/null 2>&1 && echo " + agentProbePresent +
		" || echo " + agentProbeAbsent
	return "sh -c " + shellQuote(inner)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// probeAgent checks whether agentType's CLI is on target and records the
// answer (registry.TargetAgent). An agent-type with no Binary is never
// probed and counts as available. A probe that fails to run — the target
// unreachable, say — is an error and records nothing: "couldn't look" is
// not "absent".
func (r *Router) probeAgent(ctx context.Context, target *registry.Target, agentType string, entry AgentType) (bool, error) {
	if entry.Binary == "" {
		return true, nil
	}
	exec, err := r.newExecutor(target)
	if err != nil {
		return false, fmt.Errorf("probe agent %q: %w", agentType, err)
	}
	out, err := exec.RunOnce(ctx, agentProbeCommand(entry.Binary))
	if err != nil {
		return false, fmt.Errorf("probe agent %q on target %q: %w", agentType, target.Name, err)
	}
	var available bool
	switch {
	case strings.Contains(out, agentProbePresent):
		available = true
	case strings.Contains(out, agentProbeAbsent):
	default:
		return false, fmt.Errorf("probe agent %q on target %q: unexpected output %q", agentType, target.Name, out)
	}
	if err := r.store.SetTargetAgent(ctx, &registry.TargetAgent{
		TargetID: target.ID, AgentType: agentType, Available: available, CheckedAt: time.Now().UTC(),
	}); err != nil {
		return false, fmt.Errorf("probe agent %q: record result: %w", agentType, err)
	}
	r.logger.Info("agent probed", "target_id", target.ID, "agent_type", agentType, "available", available)
	return available, nil
}

// requireAgent probes for agentType on target, returning an
// *AgentUnavailableError if it isn't there.
func (r *Router) requireAgent(ctx context.Context, target *registry.Target, agentType string) error {
	entry, err := r.agentTypes.Get(agentType)
	if err != nil {
		return err
	}
	available, err := r.probeAgent(ctx, target, agentType, entry)
	if err != nil {
		return err
	}
	if !available {
		return &AgentUnavailableError{AgentType: agentType, TargetID: target.ID, TargetName: target.Name}
	}
	return nil
}

// RefreshTargetAgents re-probes every agent-type that declares a Binary
// on targetID and returns the recorded results, ordered by agent type —
// the on-demand refresh behind the API (LOOM-71). An unknown target is
// registry.ErrNotFound.
func (r *Router) RefreshTargetAgents(ctx context.Context, targetID string) ([]*registry.TargetAgent, error) {
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("router: refresh target agents: %w", err)
	}
	names := make([]string, 0, len(r.agentTypes))
	for name, entry := range r.agentTypes {
		if name != "" && entry.Binary != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]*registry.TargetAgent, 0, len(names))
	for _, name := range names {
		available, err := r.probeAgent(ctx, target, name, r.agentTypes[name])
		if err != nil {
			return nil, fmt.Errorf("router: refresh target agents: %w", err)
		}
		out = append(out, &registry.TargetAgent{TargetID: target.ID, AgentType: name, Available: available})
	}
	return out, nil
}

// agentAvailability reads a target's recorded probe results into
// TargetSnapshot.Agents' shape.
func (r *Router) agentAvailability(ctx context.Context, targetID string) (map[string]bool, error) {
	rows, err := r.store.ListTargetAgents(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(rows))
	for _, a := range rows {
		out[a.AgentType] = a.Available
	}
	return out, nil
}

// Output quoted back into chat or an error — an exited agent's last
// words, a command's output — is bounded so a runaway process can't flood
// the conversation.
const (
	quotedOutputMaxLines = 40
	quotedOutputMaxBytes = 4000
)

// quoteOutput keeps the end of output (where an error usually is) within
// the quoted-output bounds, marking anything dropped.
func quoteOutput(output string) string {
	output = strings.TrimRight(output, " \t\n")
	truncated := false
	if lines := strings.Split(output, "\n"); len(lines) > quotedOutputMaxLines {
		output = strings.Join(lines[len(lines)-quotedOutputMaxLines:], "\n")
		truncated = true
	}
	if len(output) > quotedOutputMaxBytes {
		output = output[len(output)-quotedOutputMaxBytes:]
		truncated = true
	}
	if truncated {
		output = "[… earlier output truncated]\n" + output
	}
	return output
}

// redactSecrets replaces every credential value the vault would inject
// for workspaceID + agentType with a placeholder, so a process that
// echoes its own key (an auth error quoting it, say) can't put it into
// chat or the logs. Values too short to be a meaningful secret are left
// alone rather than mangling ordinary text.
func (r *Router) redactSecrets(ctx context.Context, workspaceID, agentType, text string) string {
	secrets, err := r.creds.Resolve(ctx, workspaceID, agentType)
	if err != nil {
		// Can't tell what to scrub: say so rather than risk a leak.
		return "[output withheld: credentials could not be resolved to redact it]"
	}
	return redactValues(text, secrets)
}

func redactValues(text string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) >= 6 {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return text
}
