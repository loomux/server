// Package agents holds the agent-type adapters: what Loomux knows about
// launching each supported agent CLI (design spec §6 — "each registered
// agent-type supplies a launch command template and a
// completion-detection adapter"). app.DefaultAgentTypes registers these;
// router only ever sees the resulting router.AgentType values.
//
// Everything an adapter needs on the target is passed per launch — CLI
// flags and the LOOMUX_* env vars router.launchAgent sets — never by
// editing the target's own agent config (~/.claude/settings.json,
// ~/.codex/config.toml). Targets can be shared work hosts whose agent
// config belongs to their user (LOOM-75).
package agents

import (
	"encoding/json"
	"strings"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/router"
)

// touchMarker is the shell snippet both adapters' completion hooks run:
// create the marker's directory if it's gone (router creates it 0700
// before launch; umask 077 keeps a re-created one private too) and touch
// the marker. A no-op
// when LOOMUX_MARKER_PATH is unset, so the hook is harmless in any
// process that inherits it without being a Loomux task.
const touchMarker = `[ -z "$LOOMUX_MARKER_PATH" ] || { umask 077 && mkdir -p "$(dirname "$LOOMUX_MARKER_PATH")" && touch "$LOOMUX_MARKER_PATH"; }`

// The version check and the launch both run against the absolute path
// probed for Binary on the target (LOOM-71/79), so a CLI that's only in
// ~/.local/bin (claude on sc1) is still found.
//
// Minimum CLI versions: the oldest releases these launch flags were
// verified against (2026-10-02, a real turn each, marker written on
// completion). Older releases may well work, but launch refuses them
// rather than risk a turn whose hook silently never fires — the marker
// wait would then hang. Raise or lower these only after verifying.
const (
	claudeCodeMinVersion = "2.1.0"
	codexMinVersion      = "0.150.0"
)

// ClaudeCode is the "claude-code" agent-type: Claude Code's interactive
// CLI, completion via a Stop hook.
//
// The hook is passed with --settings, which Claude Code layers on top of
// the user's own settings files for this process only; hooks from all
// layers run, so the user's own hooks keep working and their files are
// never touched. Stop fires once per finished turn, which is exactly the
// TierMarker signal.
func ClaudeCode() router.AgentType {
	return router.AgentType{
		AgentConfig:        completion.AgentConfig{Tier: completion.TierMarker},
		LaunchTemplate:     "claude",
		Binary:             "claude",
		CompletionHookArgs: []string{"--settings", claudeStopHookSettings()},
		VersionCheck: &router.VersionCheck{
			Command:  "claude --version",
			Parse:    router.ExtractDottedVersion,
			Min:      claudeCodeMinVersion,
			Requires: "completion hooks (--settings Stop hook)",
		},
		Install: &router.AgentInstall{
			// Anthropic's native installer: puts claude in ~/.local/bin,
			// no Node.js needed.
			Command: "curl -fsSL https://claude.ai/install.sh | bash",
			Login: "attach to the target and run `claude` once to complete its /login, or store an " +
				"ANTHROPIC_API_KEY credential for the claude-code agent type",
		},
	}
}

func claudeStopHookSettings() string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []hook `json:"hooks"`
	}
	settings := map[string]map[string][]matcher{
		"hooks": {"Stop": {{Hooks: []hook{{Type: "command", Command: touchMarker}}}}},
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep "&&" readable in ps/tmux output
	if err := enc.Encode(settings); err != nil {
		panic(err) // static data; cannot fail
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Codex is the "codex" agent-type: OpenAI's Codex interactive CLI,
// completion via its notify program.
//
// Codex runs the notify argv after each event with the event JSON
// appended as the last argument; the script touches the marker only for
// agent-turn-complete. The -c override applies to this process only —
// but it replaces, for this process, any notify the user configured
// themselves (Codex has one notify program, not a list).
func Codex() router.AgentType {
	return router.AgentType{
		AgentConfig:        completion.AgentConfig{Tier: completion.TierMarker},
		LaunchTemplate:     "codex",
		Binary:             "codex",
		CompletionHookArgs: []string{"-c", "notify=" + tomlStringArray(codexNotifyArgv())},
		VersionCheck: &router.VersionCheck{
			Command:  "codex --version",
			Parse:    router.ExtractDottedVersion,
			Min:      codexMinVersion,
			Requires: "completion hooks (-c notify)",
		},
		Install: &router.AgentInstall{
			// Needs Node.js and npm on the target; on a target without
			// them the install fails and its output says so.
			Command: "npm install -g @openai/codex",
			Login: "attach to the target and run `codex login`, or store an OPENAI_API_KEY credential " +
				"for the codex agent type",
		},
	}
}

func codexNotifyArgv() []string {
	// sh -c SCRIPT NAME EVENT: NAME becomes $0, Codex's event JSON $1.
	script := `case "$1" in *'"agent-turn-complete"'*) ` + touchMarker + ` ;; esac`
	return []string{"sh", "-c", script, "loomux-notify"}
}

// tomlStringArray renders ss as a TOML array of basic strings — the form
// Codex's -c parses as TOML. Only `\` and `"` need escaping for the
// printable-ASCII strings used here.
func tomlStringArray(ss []string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = `"` + esc.Replace(s) + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
