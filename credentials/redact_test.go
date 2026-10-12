package credentials_test

import (
	"context"
	"errors"
	"strings"
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

// A value holding raw invalid UTF-8 is still redacted verbatim, with or
// without whitespace in the text (#319 re-review).
func TestRedactValues_InvalidUTF8Value(t *testing.T) {
	secrets := map[string]string{"raw": "tok\xffen-ABCDEF123456"}
	for _, in := range []string{"x=tok\xffen-ABCDEF123456;", "x = tok\xffen-ABCDEF123456 ;"} {
		if got := credentials.RedactValues(in, secrets); strings.Contains(got, "ABCDEF") {
			t.Errorf("RedactValues(%q) = %q, want the value redacted", in, got)
		}
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

// Two values that only partly overlap — one's tail is the other's head —
// are redacted as their union: replacing either first would cut the
// other's match in two and leave the rest of it in the text (LOOM-184).
func TestRedactValues_PartiallyOverlappingValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets map[string]string
		in      string
	}{
		// Equal length, so neither is "longest first".
		{"exact", map[string]string{"a": "abcdefGHIJ", "b": "GHIJklmnop"}, "x abcdefGHIJklmnop y"},
		// Different lengths, the longer second.
		{"exact uneven", map[string]string{"a": "abcdefGHIJ", "b": "GHIJklmnopqrst"}, "x abcdefGHIJklmnopqrst y"},
		// Twelve bytes and more, so whitespace-split matching applies,
		// with the text wrapped inside the overlap.
		{"split", map[string]string{"a": "sk-AAAAAA-SHARED1", "b": "SHARED1-BBBBBBBB"}, "x sk-AAAAAA-SHA\nRED1-BBBBBBBB y"},
		// Three in a chain.
		{"chain", map[string]string{"a": "aaaaa11111", "b": "11111bbbbb", "c": "bbbbbccccc"}, "x aaaaa11111bbbbbccccc y"},
	} {
		for i := 0; i < 50; i++ {
			if got := credentials.RedactValues(tc.in, tc.secrets); got != "x [redacted] y" {
				t.Fatalf("%s, round %d: RedactValues = %q, want %q", tc.name, i, got, "x [redacted] y")
			}
		}
	}
}

// A value that overlaps itself in the text (a run longer than the value)
// leaves nothing of it behind either.
func TestRedactValues_SelfOverlappingValue(t *testing.T) {
	got := credentials.RedactValues("x 010101010101 y", map[string]string{"k": "0101010101"})
	if got != "x [redacted] y" {
		t.Fatalf("RedactValues = %q, want %q", got, "x [redacted] y")
	}
}
