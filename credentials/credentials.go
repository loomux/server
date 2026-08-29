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

const (
	specGlobal specificity = iota
	specAgentTypeOnly
	specWorkspaceOnly
	specWorkspaceAndAgentType
)

// Resolve returns the decrypted secrets applicable to workspaceID +
// agentType (name -> value), applying this precedence — most specific
// wins:
//
//  1. workspace AND agent-type both match
//  2. workspace matches, agent-type unscoped
//  3. agent-type matches, workspace unscoped
//  4. fully global (neither scoped)
//
// When two candidates for the same name are equally specific at level 2
// vs. 3 (one workspace-only, one agent-type-only), the workspace-scoped
// one wins — an arbitrary but deterministic tie-break: a workspace is a
// more concrete unit than an agent-type category. A further tie-break
// (genuine duplicates at the exact same scope, possible since SQLite's
// UNIQUE constraint can't dedupe two NULL-workspace_id rows) goes to
// whichever was updated most recently.
func (r *Resolver) Resolve(ctx context.Context, workspaceID, agentType string) (map[string]string, error) {
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
	case c.AgentType != "":
		return specAgentTypeOnly
	default:
		return specGlobal
	}
}
