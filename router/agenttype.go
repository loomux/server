// Package router wires the workspace registry, orchestrator, completion
// detection, and credential vault together into the dispatch pipeline
// (design spec §2, §6): given an incoming chat message, decide which
// workspace to use (or provision a new one), launch or provision via
// the orchestrator, and relay the result back. See router/README.md.
package router

import (
	"fmt"
	"strings"

	"github.com/Loomux/server/completion"
)

// AgentType is a full registered agent-type entry (design spec §6:
// "each registered agent-type supplies a launch command template and a
// completion-detection adapter"). It composes completion.AgentConfig
// (LOOM-6's completion-detection half) rather than duplicating its
// fields, so an agent-type's tier/timeout has exactly one source of
// truth.
type AgentType struct {
	completion.AgentConfig
	// LaunchTemplate is the already-runnable command that starts this
	// agent-type's CLI (e.g. "claude"). A literal string, not a
	// templating DSL with substitutable placeholders — nothing in this
	// package needs per-launch parameterization inside the command
	// itself (the workspace directory is handled separately by
	// NewSession's own dir parameter; credentials are prefixed
	// separately via credentials.ShellEnvPrefix, not templated in).
	LaunchTemplate string
	// VersionCheck, if set, is run before every launch of this
	// agent-type (design spec §10 axis 3). nil means no check is
	// enforced.
	VersionCheck *VersionCheck
	// CompletionHookArgs are extra CLI arguments, appended after
	// LaunchTemplate on every launch, that make the agent CLI itself
	// touch $LOOMUX_MARKER_PATH when a turn finishes (LOOM-75) — e.g.
	// claude's --settings carrying a Stop hook, codex's -c notify=…. They
	// are scoped to the one launched process: nothing is written to the
	// target's own agent config, which on a shared host belongs to
	// someone else. Each element is shell-quoted as one word. Empty means
	// the agent signals some other way (or, for TierIdle, not at all).
	CompletionHookArgs []string
	// Profile is how the CLI is started unattended: permission/sandbox
	// flags, workspace pre-trust, first prompt as an argument (LOOM-78).
	// The zero value adds nothing to the command.
	Profile LaunchProfile
	// Binary is the agent CLI's executable name. When set, the target is
	// probed for it (`command -v`) before every fresh launch and before a
	// workspace is provisioned for it, and the result is recorded per
	// target (LOOM-71). Empty means the agent-type is never probed.
	Binary string
	// Install, if set, is how to put the CLI on a target that lacks it —
	// offered to the user, and run only on their explicit confirmation.
	Install *AgentInstall
}

// AgentInstall is an agent-type's install recipe (LOOM-71). Both fields
// are fixed configuration: the routing model never supplies or alters
// either, so what an install confirmation runs is exactly what the offer
// showed.
type AgentInstall struct {
	// Command installs the CLI. It runs verbatim on the target as a
	// tracked command task (registry.TaskKindCommand).
	Command string
	// Login is the human step the CLI needs after installing before it
	// can run unattended (e.g. "run `codex login` on the target"), told
	// to the user alongside the offer and again after a successful
	// install. Loomux never performs it.
	Login string
}

// AgentTypeRegistry maps agent-type name to its AgentType entry. An
// entry keyed by the empty string configures completion detection for
// every shell-kind task (provisioning), since registry.Task.AgentType is
// always "" for those and completion.Detector looks tasks up by exactly
// that field — the only way to reach shell tasks through this
// mechanism.
type AgentTypeRegistry map[string]AgentType

// CompletionConfig projects this registry into the narrower
// completion.Config shape completion.NewDetector needs. Always derived
// from the registry, never hand-maintained separately.
func (r AgentTypeRegistry) CompletionConfig() completion.Config {
	cfg := make(completion.Config, len(r))
	for name, at := range r {
		cfg[name] = at.AgentConfig
	}
	return cfg
}

// Get returns the full AgentType entry for agentType, or an error if it
// isn't registered.
func (r AgentTypeRegistry) Get(agentType string) (AgentType, error) {
	at, ok := r[agentType]
	if !ok {
		return AgentType{}, fmt.Errorf("router: unknown agent type %q", agentType)
	}
	return at, nil
}

// LaunchCommand returns the resolved launch command for agentType, or
// an error if it isn't registered.
func (r AgentTypeRegistry) LaunchCommand(agentType string) (string, error) {
	at, err := r.Get(agentType)
	if err != nil {
		return "", err
	}
	return at.LaunchTemplate, nil
}

// launchCommand is the agent CLI invocation launchAgent runs (before any
// env prefix), each argument shell-quoted: LaunchTemplate, the profile's
// permission args, its trust args for workspaceDir, CompletionHookArgs,
// then — when the profile passes the first prompt as an argument and
// there is one — "--" and the prompt. "--" keeps a message that starts
// with "-" from being read as a flag.
func (at AgentType) launchCommand(workspaceDir, prompt string) string {
	args := append([]string{}, at.Profile.PermissionArgs...)
	if at.Profile.TrustArgs != nil {
		args = append(args, at.Profile.TrustArgs(workspaceDir)...)
	}
	args = append(args, at.CompletionHookArgs...)
	if at.Profile.PromptAsArg && prompt != "" {
		args = append(args, "--", prompt)
	}

	var b strings.Builder
	b.WriteString(at.LaunchTemplate)
	for _, arg := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
	}
	return b.String()
}
