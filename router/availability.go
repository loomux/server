package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
)

// What an agent CLI probe prints: either the absent sentinel, or the
// resolved absolute path and the first line of its --version. Sentinels
// rather than the exit status: TargetExecutor.RunOnce reports a non-zero
// exit as an error, which would leave "not installed" indistinguishable
// from "couldn't run the probe".
const (
	agentProbePathPrefix    = "loomux-agent-path:"
	agentProbeVersionPrefix = "loomux-agent-version:"
	agentProbeAbsent        = "loomux-agent-absent"
)

// agentSearchDirs are where agent CLIs commonly install outside a
// non-interactive shell's PATH (LOOM-79: on sc1, claude is only in
// ~/.local/bin), checked when neither the login shell nor the plain PATH
// finds the binary.
var agentSearchDirs = []string{`$HOME/.local/bin`, `$HOME/.npm-global/bin`, `$HOME/bin`, `/usr/local/bin`, `/opt/homebrew/bin`}

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

// agentProbeCommand resolves binary to an absolute path on a target
// (LOOM-79), in POSIX sh whatever the target's login shell is. It looks
// where a person at that machine would find it first — their login
// shell's PATH ($SHELL, then bash) — then the non-interactive PATH, then
// agentSearchDirs; and reports the --version of exactly the path it found,
// since that is the path the launch will run. Output: the absent sentinel,
// or the path and version lines.
func agentProbeCommand(binary string) string {
	lookup := shellQuote("command -v " + shellQuote(binary))
	var dirs []string
	for _, d := range agentSearchDirs {
		dirs = append(dirs, `"`+d+`"`)
	}
	script := `b=` + shellQuote(binary) + `
p=
for s in "${SHELL:-}" bash; do
  [ -n "$s" ] && command -v "$s" >/dev/null 2>&1 || continue
  p=$("$s" -lc ` + lookup + ` </dev/null 2>/dev/null | tail -n 1)
  case "$p" in /*) break ;; *) p= ;; esac
done
if [ -z "$p" ]; then
  p=$(command -v "$b" 2>/dev/null)
  case "$p" in /*) ;; *) p= ;; esac
fi
if [ -z "$p" ]; then
  for d in ` + strings.Join(dirs, " ") + `; do
    if [ -x "$d/$b" ]; then p="$d/$b"; break; fi
  done
fi
if [ -z "$p" ]; then echo ` + agentProbeAbsent + `; exit 0; fi
if command -v timeout >/dev/null 2>&1; then t="timeout 10"; else t=; fi
v=$($t "$p" --version </dev/null 2>&1 | head -n 1)
echo "` + agentProbePathPrefix + `$p"
echo "` + agentProbeVersionPrefix + `$v"
`
	return "sh -c " + shellQuote(script)
}

// parseAgentProbe reads agentProbeCommand's output.
func parseAgentProbe(out string) (path, version string, err error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == agentProbeAbsent:
			return "", "", nil
		case strings.HasPrefix(line, agentProbePathPrefix):
			path = strings.TrimPrefix(line, agentProbePathPrefix)
		case strings.HasPrefix(line, agentProbeVersionPrefix):
			version = strings.TrimPrefix(line, agentProbeVersionPrefix)
		}
	}
	if !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("unexpected output %q", out)
	}
	const maxVersion = 200
	if len(version) > maxVersion {
		version = version[:maxVersion]
	}
	return path, version, nil
}

// withResolvedBinary rewrites command — an agent-type's launch template or
// version-check command — to run the probed absolute path instead of
// binary looked up on PATH (LOOM-79). A command that doesn't start with
// binary is returned unchanged, as is any command when nothing was
// resolved.
func withResolvedBinary(command, binary, path string) string {
	if path == "" || binary == "" {
		return command
	}
	if command == binary {
		return shellQuote(path)
	}
	if strings.HasPrefix(command, binary+" ") {
		return shellQuote(path) + command[len(binary):]
	}
	return command
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// probeAgent resolves agentType's CLI on target and records the result
// (registry.TargetAgent: availability, absolute path, version). An
// agent-type with no Binary is never probed and counts as available, with
// no path. A probe that fails to run — the target unreachable, say — is
// an error and records nothing: "couldn't look" is not "absent".
func (r *Router) probeAgent(ctx context.Context, target *registry.Target, agentType string, entry AgentType) (*registry.TargetAgent, error) {
	if entry.Binary == "" {
		return &registry.TargetAgent{TargetID: target.ID, AgentType: agentType, Available: true}, nil
	}
	exec, err := r.newExecutor(target)
	if err != nil {
		return nil, fmt.Errorf("probe agent %q: %w", agentType, err)
	}
	out, err := exec.RunOnce(ctx, agentProbeCommand(entry.Binary))
	if err != nil {
		return nil, fmt.Errorf("probe agent %q on target %q: %w", agentType, target.Name, err)
	}
	path, version, err := parseAgentProbe(out)
	if err != nil {
		return nil, fmt.Errorf("probe agent %q on target %q: %w", agentType, target.Name, err)
	}
	rec := &registry.TargetAgent{
		TargetID: target.ID, AgentType: agentType, Available: path != "",
		Path: path, Version: version, CheckedAt: time.Now().UTC(),
	}
	if err := r.store.SetTargetAgent(ctx, rec); err != nil {
		return nil, fmt.Errorf("probe agent %q: record result: %w", agentType, err)
	}
	r.logger.Info("agent probed", "target_id", target.ID, "agent_type", agentType, "available", rec.Available,
		"path", path, "version", version)
	return rec, nil
}

// requireAgent probes for agentType on target, returning its record, or
// an *AgentUnavailableError if it isn't there.
func (r *Router) requireAgent(ctx context.Context, target *registry.Target, agentType string) (*registry.TargetAgent, error) {
	entry, err := r.agentTypes.Get(agentType)
	if err != nil {
		return nil, err
	}
	rec, err := r.probeAgent(ctx, target, agentType, entry)
	if err != nil {
		return nil, err
	}
	if !rec.Available {
		return nil, &AgentUnavailableError{AgentType: agentType, TargetID: target.ID, TargetName: target.Name}
	}
	return rec, nil
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
		rec, err := r.probeAgent(ctx, target, name, r.agentTypes[name])
		if err != nil {
			return nil, fmt.Errorf("router: refresh target agents: %w", err)
		}
		out = append(out, rec)
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

// redactAllSecrets replaces every credential value in the vault — any
// scope, any agent type — with a placeholder: for output from something
// that ran with no credentials injected (a provisioning recipe, an
// install) but could still print one. If the vault can't be read, the
// output is withheld rather than shown unscrubbed.
func (r *Router) redactAllSecrets(ctx context.Context, text string) string {
	creds, err := r.store.ListCredentials(ctx)
	if err != nil {
		return "[output withheld: credentials could not be read to redact it]"
	}
	values := make(map[string]string, len(creds))
	for _, c := range creds {
		values[c.ID] = c.Value
	}
	return redactValues(text, values)
}

func redactValues(text string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) >= 6 {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return text
}
