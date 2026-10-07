package credentials

import (
	"strings"
	"testing"
)

// LOOM-108: secrets Loomux doesn't hold in its vault (the agent's own
// tokens, a key in a file it printed) are recognised by shape before
// output leaves for the relay model.
func TestRedactPatterns(t *testing.T) {
	secrets := []string{
		"ghp_1234567890abcdefghijABCDEFGHIJ123456",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWX",
		"sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"AKIAABCDEFGHIJKLMNOP",
		"xoxb-123456789012-1234567890123-abcdefghijklmnopqrstuvwx",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"gsk_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP",
	}
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU=\n-----END OPENSSH PRIVATE KEY-----"
	text := "build ok\n" + strings.Join(secrets, "\n") + "\n" + pem + "\n" +
		"export DATABASE_PASSWORD=hunter2hunter2\nAPI_KEY: abc123def456\nmy_token = 'q1w2e3r4t5'\n" +
		"normal line with sk- and eyJ and a hash 3f2a9c1d\n"

	out := RedactPatterns(text)
	for _, s := range append(secrets, "b3BlbnNzaC1rZXktdjEAAAAABG5vbmU=", "hunter2hunter2", "abc123def456", "q1w2e3r4t5") {
		if strings.Contains(out, s) {
			t.Errorf("%q survived:\n%s", s, out)
		}
	}
	for _, keep := range []string{"build ok", "DATABASE_PASSWORD=", "API_KEY:", "normal line with sk- and eyJ and a hash 3f2a9c1d"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q was lost:\n%s", keep, out)
		}
	}
}

// A credential in a URL's userinfo is redacted whatever its shape, the
// scheme and host kept; an SSH URL's plain "git" user is left alone.
func TestRedactPatterns_URLUserinfo(t *testing.T) {
	text := "git clone https://deploy:s3cr3t-Pa55@git.example.com/r.git\n" +
		"remote https://opaque-token-value-123@host.example/x\n" +
		"ssh://git@github.com/org/repo.git and git@github.com:org/repo.git\n" +
		"see https://example.com/a@b and mailto:me@example.com"
	out := RedactPatterns(text)
	for _, secret := range []string{"s3cr3t-Pa55", "deploy:", "opaque-token-value-123"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q survived:\n%s", secret, out)
		}
	}
	for _, keep := range []string{"https://[redacted]@git.example.com/r.git", "https://[redacted]@host.example/x",
		"ssh://git@github.com/org/repo.git", "git@github.com:org/repo.git", "https://example.com/a@b", "mailto:me@example.com"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q was lost:\n%s", keep, out)
		}
	}
}
