package credentials

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// validEnvName matches POSIX-safe shell/env variable identifiers.
var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ShellEnvPrefix turns a resolved secret map into a POSIX-shell-safe
// "VAR='value' " prefix, ready to prepend to a launch command (e.g.
// credentials.ShellEnvPrefix(secrets) + agentCommand). Each value is
// single-quote-escaped the same way LOOM-4's SSH remote command
// construction avoids injection; each name is validated against a
// strict identifier pattern and rejected otherwise — unlike a value, a
// name can't be quoted into safety (a "VAR=value" shell assignment
// requires VAR to already be a syntactically valid identifier), so an
// unvalidated name is a real injection vector, not just a formatting
// nicety. Keys are sorted for deterministic, reproducible output.
func ShellEnvPrefix(secrets map[string]string) (string, error) {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		if !validEnvName.MatchString(name) {
			return "", fmt.Errorf("credentials: %q is not a valid environment variable name", name)
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(shellQuote(secrets[name]))
		b.WriteByte(' ')
	}
	return b.String(), nil
}

// shellQuote POSIX-single-quotes s, safe to embed in a shell command.
// Written fresh here rather than reusing targets' unexported equivalent
// — three lines, not worth a shared-util package for.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validTaskID bounds the one name EnvFile puts into a path.
var validTaskID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// EnvFile carries secrets to a launched process without putting them on
// any command line (LOOM-113): a "VAR='value' " prefix sits in the tmux
// pane's start command, where tmux keeps it and ps shows it to every
// user on the target.
//
// Write is a script to run with TargetExecutor.RunOnce, which sends it
// as stdin to sh: it creates the file, mode 0600 in a 0700 directory,
// from a heredoc, so the values are only ever data. Source is the
// command-line prefix that exports them into the shell and deletes the
// file before the agent starts; it names only the path. Remove is a
// script that deletes the file, for a launch that failed before Source
// ran.
type EnvFile struct {
	Write  string
	Source string
	Remove string
}

// NewEnvFile builds the EnvFile for taskID's secrets.
func NewEnvFile(taskID string, secrets map[string]string) (EnvFile, error) {
	if !validTaskID.MatchString(taskID) {
		return EnvFile{}, fmt.Errorf("credentials: task id %q can't name an env file", taskID)
	}
	// Names are checked as for ShellEnvPrefix.
	if _, err := ShellEnvPrefix(secrets); err != nil {
		return EnvFile{}, err
	}
	// One assignment per line, each value single-quoted: sourcing it with
	// set -a exports every one. The heredoc's terminator is random, so no
	// value — even one containing newlines — can end it early.
	var lines strings.Builder
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines.WriteString(name + "=" + shellQuote(secrets[name]) + "\n")
	}
	terminator := "LOOMUX_ENV_" + randomToken()
	for strings.Contains(lines.String(), terminator) {
		terminator = "LOOMUX_ENV_" + randomToken()
	}
	dir := `"$HOME/.loomux/env"`
	path := `"$HOME/.loomux/env/` + taskID + `"`
	return EnvFile{
		Write: "umask 077 && mkdir -p " + dir + " && chmod 700 " + dir + " && cat > " + path + " <<'" + terminator + "'\n" +
			lines.String() + terminator + "\n",
		Source: "set -a; . " + path + "; set +a; rm -f " + path + "; ",
		Remove: "rm -f " + path,
	}, nil
}
