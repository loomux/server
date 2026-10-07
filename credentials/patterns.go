package credentials

import "regexp"

// tokenPatterns are the shapes of secrets that aren't in Loomux's vault
// but turn up in an agent's terminal: its own tokens, a key it printed
// from a file (LOOM-108). Each match is replaced whole.
var tokenPatterns = []*regexp.Regexp{
	// PEM private keys, header to footer.
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// GitHub tokens (classic and fine-grained).
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b`),
	// OpenAI / Anthropic / Groq style keys.
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gsk_[A-Za-z0-9]{20,})`),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	// JWTs: three base64url parts, the first two JSON objects.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// assignmentPattern is NAME=value or NAME: value where the name says it
// holds a secret; group 1 (the name and separator) is kept.
var assignmentPattern = regexp.MustCompile(
	`(?i)\b([A-Za-z0-9_]*(?:api_?key|secret|token|passw(?:or)?d|passwd|private_?key|access_?key)[A-Za-z0-9_]*\s*[=:]\s*)` +
		`("[^"\n]+"|'[^'\n]+'|[^\s"']+)`)

// urlUserinfoPattern is a URL's userinfo (scheme://user:password@host, or
// a bare token as the user): group 1, the scheme, is kept. Only "git", the
// usual SSH user, is left as it is.
var urlUserinfoPattern = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)([^/@\s]+)@`)

// RedactPatterns replaces anything in text shaped like a secret with
// "[redacted]": known token formats, private keys, credentials in a URL,
// and the value of an
// assignment whose name says it's a secret. It complements RedactValues,
// which removes what the vault holds; neither alone is enough for text
// leaving Loomux.
func RedactPatterns(text string) string {
	for _, p := range tokenPatterns {
		text = p.ReplaceAllString(text, "[redacted]")
	}
	text = urlUserinfoPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := urlUserinfoPattern.FindStringSubmatch(m)
		if sub[2] == "git" {
			return m
		}
		return sub[1] + "[redacted]@"
	})
	return assignmentPattern.ReplaceAllString(text, "${1}[redacted]")
}
