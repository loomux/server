package router

import (
	"fmt"
	"sort"
)

// LaunchProfile is how an agent-type's CLI is started unattended
// (LOOM-78): what it may do without asking, whether its workspace is
// pre-trusted, and how the first turn's message reaches it. The
// production defaults live with each adapter in package agents, where
// every flag is documented with what it permits. Operators override them
// with ApplyProfileOverrides (app: LOOMUX_AGENT_PROFILES). Per-target
// policy is a later step (LOOM-89).
type LaunchProfile struct {
	// PermissionArgs set the agent's permission and sandbox posture
	// (e.g. claude --permission-mode acceptEdits). Empty means the CLI's
	// own default, which for an interactive CLI usually means asking a
	// human before acting.
	PermissionArgs []string
	// TrustArgs, if set, returns arguments that mark workspaceDir as
	// trusted for this launch only, so the CLI doesn't stop at a "do you
	// trust this folder?" dialog. Must not persist anything on the
	// target. nil means the CLI has no per-launch way to do this.
	TrustArgs func(workspaceDir string) []string
	// PromptAsArg passes a fresh task's first message as a trailing
	// positional argument (after "--") instead of typing it into the
	// pane with send-keys, which races the TUI's startup and can be
	// swallowed by a startup dialog. Later turns still use send-keys:
	// the previous turn's completion signal shows the agent is ready.
	//
	// The message then sits in the agent process's argv, which other
	// users of a shared target can read (ps, /proc/<pid>/cmdline).
	PromptAsArg bool
}

// ProfileOverride replaces parts of an agent-type's LaunchProfile. A nil
// field keeps the default; a non-nil one replaces it (so an empty
// PermissionArgs slice means "no permission flags at all").
type ProfileOverride struct {
	PermissionArgs *[]string `json:"permission_args,omitempty"`
	// PreTrust false drops the profile's TrustArgs. true keeps them; it
	// can't add a trust mechanism to a CLI that has none.
	PreTrust    *bool `json:"pre_trust,omitempty"`
	PromptAsArg *bool `json:"prompt_as_arg,omitempty"`
}

// ApplyProfileOverrides applies overrides (keyed by agent-type name) to
// r in place. An override for an unregistered agent-type is an error, so
// a typo can't silently leave the default posture in force.
func (r AgentTypeRegistry) ApplyProfileOverrides(overrides map[string]ProfileOverride) error {
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		at, ok := r[name]
		if !ok || name == "" {
			return fmt.Errorf("router: launch profile override for unknown agent type %q", name)
		}
		o := overrides[name]
		if o.PermissionArgs != nil {
			at.Profile.PermissionArgs = append([]string{}, (*o.PermissionArgs)...)
		}
		if o.PreTrust != nil && !*o.PreTrust {
			at.Profile.TrustArgs = nil
		}
		if o.PromptAsArg != nil {
			at.Profile.PromptAsArg = *o.PromptAsArg
		}
		r[name] = at
	}
	return nil
}
