package router

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// checkAgentChoice settles a decision's agent_type against what the
// target is recorded to have (LOOM-88), before anything is written. The
// routing model is told availability, but a decision naming an agent the
// target is recorded not to have is still corrected here:
//
//   - the user named that agent: left alone — the pre-launch probe turns
//     it into the LOOM-71 install offer, which is what was asked for;
//   - otherwise, if another agent-type is recorded available on the same
//     target, it is used instead, and substitutedFrom names the one it
//     replaced;
//   - otherwise left alone, for the install offer.
//
// Recorded availability only: an agent never probed on the target is
// left to the pre-launch probe, exactly as before.
func (r *Router) checkAgentChoice(ctx context.Context, d Decision, message string) (out Decision, substitutedFrom, targetName string) {
	if d.Action != ActionUseWorkspace && d.Action != ActionProvisionWorkspace {
		return d, "", ""
	}
	targetID := d.NewWorkspace.TargetID
	if d.Action == ActionUseWorkspace {
		ws, err := r.store.GetWorkspace(ctx, d.WorkspaceID)
		if err != nil {
			return d, "", ""
		}
		targetID = ws.TargetID
	}
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return d, "", ""
	}
	agents, _, err := r.agentAvailability(ctx, target.ID)
	if err != nil {
		return d, "", ""
	}
	if available, recorded := agents[d.AgentType]; !recorded || available {
		return d, "", ""
	}
	if entry, err := r.agentTypes.Get(d.AgentType); err == nil && AgentNamedIn(message, d.AgentType, entry) {
		return d, "", ""
	}
	names := make([]string, 0, len(agents))
	for name, ok := range agents {
		if _, err := r.agentTypes.Get(name); ok && name != "" && err == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return d, "", ""
	}
	sort.Strings(names)
	from := d.AgentType
	d.AgentType = names[0]
	return d, from, target.Name
}

// substitutionNote is the line put in front of the reply when
// checkAgentChoice swapped the agent, so the user knows which one ran.
func substitutionNote(used, from, targetName string) string {
	return fmt.Sprintf("Using %s: %s isn't installed on %s.", used, from, targetName)
}

// agentSubstitutionNote is put in front of the message the substitute
// agent receives, so it knows why it got work meant for another agent
// rather than seeing only the user's own words.
func agentSubstitutionNote(used, from, targetName string) string {
	return fmt.Sprintf("[Loomux note: this request was routed to %s, which isn't installed on %s, so you (%s) are handling it instead.]",
		from, targetName, used)
}

// AgentNamedIn reports whether message names agentType — by its
// registered name or its CLI's binary — as a whole word, ignoring case.
// Exported for tests.
func AgentNamedIn(message, agentType string, entry AgentType) bool {
	for _, name := range []string{agentType, entry.Binary} {
		if name == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)(^|[^a-z0-9_])` + regexp.QuoteMeta(strings.ToLower(name)) + `($|[^a-z0-9_])`)
		if re.MatchString(message) {
			return true
		}
	}
	return false
}
