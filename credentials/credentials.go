// Package credentials implements the "everything else" half of design
// spec §7's hybrid credential model: GitHub/GitLab PATs, MCP tokens, and
// custom API keys, resolved with workspace/agent-type scoping precedence
// on top of the vault registry.Store owns (§8 groups the registry tables
// and the credential vault under one storage interface).
//
// The other half of §7 — OAuth-native agent CLIs (Claude Code's `claude
// login`, etc.) — needs no code here: they manage their own session
// lifecycle, and are expected to already be authenticated in the
// target's HOME/config dir. See this package's README for how that
// expectation is documented.
package credentials

import (
	"context"
	"fmt"

	"github.com/Loomux/server/registry"
)

// Resolver resolves the secrets a task should have, applying scope
// precedence over registry.Store's credential records.
type Resolver struct {
	store registry.Store
}

// NewResolver constructs a Resolver.
func NewResolver(store registry.Store) *Resolver {
	return &Resolver{store: store}
}

// specificity ranks how targeted a credential's scope is. Higher wins.
type specificity int

// The scope levels, least specific first. A workspace is more concrete
// than a target (a workspace lives on one target), a target than an
// agent type; within a level, having the agent type too wins.
const (
	specGlobal specificity = iota
	specAgentTypeOnly
	specTargetOnly
	specTargetAndAgentType
	specWorkspaceOnly
	specWorkspaceAndAgentType
)

// Resolve returns the decrypted secrets applicable to workspaceID,
// targetID (the workspace's target) and agentType (name -> value),
// applying this precedence — most specific wins:
//
//  1. workspace AND agent-type both match
//  2. workspace matches, agent-type unscoped
//  3. target AND agent-type both match (LOOM-178)
//  4. target matches, agent-type unscoped
//  5. agent-type matches, workspace and target unscoped
//  6. fully global (nothing scoped)
//
// A credential scoped to another workspace or target never applies. A
// workspace-scoped credential outranks a target-scoped one even without
// the agent type (a workspace is a more concrete unit), and both outrank
// an agent-type-only one. A tie at the exact same scope (possible since
// SQLite's UNIQUE constraint can't dedupe two NULL-workspace_id rows)
// goes to whichever was updated most recently.
func (r *Resolver) Resolve(ctx context.Context, workspaceID, targetID, agentType string) (map[string]string, error) {
	all, err := r.store.ListCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("credentials: resolve: %w", err)
	}

	best := make(map[string]*registry.Credential)
	bestSpec := make(map[string]specificity)

	for _, c := range all {
		if c.WorkspaceID != "" && c.WorkspaceID != workspaceID {
			continue
		}
		if c.TargetID != "" && c.TargetID != targetID {
			continue
		}
		if c.AgentType != "" && c.AgentType != agentType {
			continue
		}

		spec := scopeSpecificity(c)
		current, ok := best[c.Name]
		if !ok {
			best[c.Name] = c
			bestSpec[c.Name] = spec
			continue
		}

		switch {
		case spec > bestSpec[c.Name]:
			best[c.Name] = c
			bestSpec[c.Name] = spec
		case spec == bestSpec[c.Name] && c.UpdatedAt.After(current.UpdatedAt):
			best[c.Name] = c
			bestSpec[c.Name] = spec
		}
	}

	out := make(map[string]string, len(best))
	for name, c := range best {
		out[name] = c.Value
	}
	return out, nil
}

func scopeSpecificity(c *registry.Credential) specificity {
	switch {
	case c.WorkspaceID != "" && c.AgentType != "":
		return specWorkspaceAndAgentType
	case c.WorkspaceID != "":
		return specWorkspaceOnly
	case c.TargetID != "" && c.AgentType != "":
		return specTargetAndAgentType
	case c.TargetID != "":
		return specTargetOnly
	case c.AgentType != "":
		return specAgentTypeOnly
	default:
		return specGlobal
	}
}
