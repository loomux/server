# credentials

Hybrid credential model. See design spec §7.

OAuth-based agent CLIs (Claude Code, etc.) manage their own session/token
lifecycle already — this component doesn't reimplement that, it only
ensures the pane's environment points at an already-authenticated config
dir. Everything else (GitHub/GitLab PATs, MCP tokens, custom API keys)
goes through an encrypted-at-rest secrets store owned here, scoped per
agent-type/workspace, decrypted only at pane-launch time.
