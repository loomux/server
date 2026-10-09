package credentials_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/registry"
)

type fakeLister struct {
	creds []*registry.Credential
	err   error
}

func (f fakeLister) ListCredentials(context.Context) ([]*registry.Credential, error) {
	return f.creds, f.err
}

func TestRedactValues(t *testing.T) {
	got := credentials.RedactValues("key sk-abc123 and abc", map[string]string{"K": "sk-abc123", "SHORT": "abc"})
	if got != "key [redacted] and abc" {
		t.Fatalf("RedactValues = %q", got)
	}
}

// A value inside another (not only a prefix) never leaves a piece of the
// longer one behind, whatever the map order (LOOM-156).
func TestRedactValues_NestedValuesLongestFirst(t *testing.T) {
	secrets := map[string]string{"inner": "XYZ123", "outer": "abcXYZ123def", "other": "zzzzzz"}
	for i := 0; i < 100; i++ {
		if got := credentials.RedactValues("t=abcXYZ123def u=XYZ123", secrets); got != "t=[redacted] u=[redacted]" {
			t.Fatalf("round %d: RedactValues = %q", i, got)
		}
	}
}

// A value broken up by whitespace (a TUI's own line wrap, or spaced
// groups) is still redacted, whole (LOOM-157).
func TestRedactValues_WhitespaceSplitValue(t *testing.T) {
	secrets := map[string]string{"K": "sk-live-ABCDEF123456", "SPACED": "9876 5432 1098", "SHORT": "ab cdef"}
	for _, tc := range []struct{ in, want string }{
		{"token: sk-live-ABC\nDEF123456 done", "token: [redacted] done"},
		{"token: sk-live-\r\n    ABCDEF\t123456", "token: [redacted]"},
		{"card 987654321098 and 9876 5432 1098", "card [redacted] and [redacted]"},
		{"sk-live and ABCDEF stay", "sk-live and ABCDEF stay"},
		// Under twelve bytes a value matches exactly, not across lines.
		{"x ab cdef y, ab\ncdef", "x [redacted] y, ab\ncdef"},
	} {
		if got := credentials.RedactValues(tc.in, secrets); got != tc.want {
			t.Errorf("RedactValues(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// One pass, longest first, covers both fixes together: a long value
// split across a line break, with a short value that is an exact
// substring of it, is redacted whole (LOOM-156 + LOOM-157 review).
func TestRedactValues_SplitLongValueWithShortSubstring(t *testing.T) {
	secrets := map[string]string{"long": "ghp_ABCDEFGHIJ0123456789", "short": "ABCDEFGHIJ"}
	for i := 0; i < 100; i++ {
		got := credentials.RedactValues("token ghp_ABCDEFGHIJ012\n3456789 and ABCDEFGHIJ", secrets)
		if got != "token [redacted] and [redacted]" {
			t.Fatalf("round %d: RedactValues = %q", i, got)
		}
	}
}

// Whitespace means the same thing when a value is stripped as when the
// text is matched: a value holding Unicode whitespace (NBSP, \v, U+3000)
// is still redacted verbatim, and when split by such whitespace (#319
// re-review).
func TestRedactValues_UnicodeWhitespace(t *testing.T) {
	secrets := map[string]string{"nbsp": "pass\u00a0word-ABCDEF12", "vt": "key\vvalue-0123456789"}
	for _, tc := range []struct{ in, want string }{
		{"a pass\u00a0word-ABCDEF12 b", "a [redacted] b"},
		{"a key\vvalue-0123456789 b", "a [redacted] b"},
		{"a password-ABC\u3000DEF12 b", "a [redacted] b"},
		{"a keyvalue-01234\u008556789 b", "a [redacted] b"},
	} {
		if got := credentials.RedactValues(tc.in, secrets); got != tc.want {
			t.Errorf("RedactValues(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A system secret (LOOM-185) goes through the same pass: wrapped across
// a line, it is still redacted whole.
func TestRedactValues_SplitSystemSecret(t *testing.T) {
	credentials.AddSystemSecret("sk-ant-loom157-SYSTEMKEY0123")
	got := credentials.RedactValues("key sk-ant-loom157-SYS\nTEMKEY0123 end", nil)
	if got != "key [redacted] end" {
		t.Fatalf("RedactValues = %q", got)
	}
}

func TestRedactAll(t *testing.T) {
	store := fakeLister{creds: []*registry.Credential{
		{ID: "a", Value: "ghp_secret1"}, {ID: "b", WorkspaceID: "ws", Value: "tok-ws-only"},
	}}
	got, err := credentials.RedactAll(context.Background(), store, "ghp_secret1 / tok-ws-only / fine")
	if err != nil || got != "[redacted] / [redacted] / fine" {
		t.Fatalf("RedactAll = %q, %v", got, err)
	}
	got, err = credentials.RedactAll(context.Background(), fakeLister{err: errors.New("no key")}, "ghp_secret1")
	if !errors.Is(err, credentials.ErrUnredactable) || got != "" {
		t.Fatalf("RedactAll with an unreadable vault = %q, %v; want no text and ErrUnredactable", got, err)
	}
}

// LOOM-185: a system secret (a router provider key) is removed by every
// scrub, with or without a vault value in the same text, and stays
// removed after more are added.
func TestRedactValues_SystemSecrets(t *testing.T) {
	credentials.AddSystemSecret("router-key-AAAA1111")
	credentials.AddSystemSecret("short") // under the minimum: ignored
	credentials.AddSystemSecret("router-key-BBBB2222")
	got := credentials.RedactValues("a router-key-AAAA1111 b router-key-BBBB2222 c short", map[string]string{"K": "vault-value"})
	if got != "a [redacted] b [redacted] c short" {
		t.Fatalf("RedactValues = %q", got)
	}
	out, err := credentials.RedactAll(context.Background(), fakeLister{}, "x router-key-AAAA1111")
	if err != nil || out != "x [redacted]" {
		t.Fatalf("RedactAll = %q, %v", out, err)
	}
}
