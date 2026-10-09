package registry

import "time"

// Credential is a secret Loomux's vault owns on behalf of a task launch
// (GitHub/GitLab PATs, MCP tokens, custom API keys — see design spec
// §7). It does NOT cover OAuth-native agent CLI sessions (Claude Code's
// `claude login`, etc.), which manage their own token lifecycle and are
// expected to already be authenticated in the target's HOME/config dir.
type Credential struct {
	ID   string
	Name string // becomes an env var name at resolution time
	// WorkspaceID scopes this credential to one workspace; empty means
	// not workspace-scoped.
	WorkspaceID string
	// AgentType scopes this credential to one agent-type; empty means
	// not agent-type-scoped.
	AgentType string
	// TargetID scopes this credential to one target (LOOM-178: a machine
	// a plugin made, whose agents all need the same token); empty means
	// not target-scoped.
	TargetID string
	// Value is plaintext at this domain-type level; a Store
	// implementation is responsible for encrypting it at rest and
	// decrypting on read.
	Value     string
	CreatedAt time.Time
	UpdatedAt time.Time
}
