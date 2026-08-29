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
