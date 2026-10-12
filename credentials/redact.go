package credentials

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/Loomux/server/registry"
)

// minRedactLen is the shortest credential value redacted: anything
// shorter would match ordinary text.
const minRedactLen = 6

// minSplitRedactLen is the shortest value (its whitespace stripped)
// also matched with whitespace inside it. A shorter one could match
// pieces of unrelated lines; it is matched exactly.
const minSplitRedactLen = 12

// systemSecrets are secrets Loomux holds for itself, outside the vault:
// the router model's provider keys (LOOM-185), from the env and stored
// through Settings, including keys rotated away from. RedactValues
// always removes them as well, so every scrub covers them.
var systemSecrets struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

// AddSystemSecret adds value to the secrets every RedactValues removes.
// Values are only ever added: a key rotated away from may still be live
// at its provider.
func AddSystemSecret(value string) {
	if len(value) < minRedactLen {
		return
	}
	systemSecrets.mu.Lock()
	defer systemSecrets.mu.Unlock()
	if systemSecrets.values == nil {
		systemSecrets.values = map[string]struct{}{}
	}
	systemSecrets.values[value] = struct{}{}
}

// RedactValues replaces every value in secrets (of at least six bytes),
// and every system secret (AddSystemSecret), found in text with
// "[redacted]". A value of at least twelve bytes also matches with
// whitespace between any of its characters — one an agent's TUI wrapped
// onto the next line, or printed in spaced groups (LOOM-157); any other
// character inside it (a TUI box's │ border) still hides it.
//
// Every match of every value is found in the original text first, and
// the union of their spans is replaced, each run of overlapping or
// touching spans by one "[redacted]": replacing one value at a time
// would cut a match of another that overlaps it — one inside another
// (LOOM-156), or one's tail the other's head (LOOM-184) — and leave the
// rest of it in the text. The result never depends on map order.
func RedactValues(text string, secrets map[string]string) string {
	type value struct{ exact, stripped string }
	values := make([]value, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= minRedactLen {
			values = append(values, value{v, stripSpace(v)})
		}
	}
	systemSecrets.mu.RLock()
	for v := range systemSecrets.values {
		values = append(values, value{v, stripSpace(v)})
	}
	systemSecrets.mu.RUnlock()
	split := strings.IndexFunc(text, unicode.IsSpace) >= 0
	// Most values aren't in the text at all: a value whose stripped form
	// isn't in the stripped text is skipped without compiling a pattern.
	var strippedText string
	if split {
		strippedText = stripSpace(text)
	}
	var spans []span
	for _, v := range values {
		switch {
		case len(v.stripped) < minSplitRedactLen:
			spans = appendExact(spans, text, v.exact)
		case !split:
			// The exact value too: stripSpace turns invalid UTF-8 into
			// U+FFFD, so the stripped form alone misses a value holding
			// raw invalid bytes.
			spans = appendExact(spans, text, v.exact)
			spans = appendExact(spans, text, v.stripped)
		case strings.Contains(strippedText, v.stripped):
			spans = appendPattern(spans, text, splitPattern(v.stripped))
		}
	}
	return replaceSpans(text, spans)
}

// span is a byte range [start, end) of text holding a secret.
type span struct{ start, end int }

// appendExact adds every occurrence of v in text, overlapping ones
// included: in "0101010" a value "01010" is at 0 and at 2.
func appendExact(spans []span, text, v string) []span {
	for from := 0; from <= len(text)-len(v); {
		i := strings.Index(text[from:], v)
		if i < 0 {
			break
		}
		spans = append(spans, span{from + i, from + i + len(v)})
		from += i + 1
	}
	return spans
}

// appendPattern is appendExact for a whitespace-split value's pattern.
func appendPattern(spans []span, text string, p *regexp.Regexp) []span {
	for from := 0; from < len(text); {
		loc := p.FindStringIndex(text[from:])
		if loc == nil {
			break
		}
		spans = append(spans, span{from + loc[0], from + loc[1]})
		from += loc[0] + 1
	}
	return spans
}

// replaceSpans replaces the union of spans in text with "[redacted]",
// once per run of overlapping or touching spans.
func replaceSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out strings.Builder
	out.Grow(len(text))
	at := 0
	for i := 0; i < len(spans); {
		start, end := spans[i].start, spans[i].end
		for i++; i < len(spans) && spans[i].start <= end; i++ {
			end = max(end, spans[i].end)
		}
		out.WriteString(text[at:start])
		out.WriteString("[redacted]")
		at = end
	}
	out.WriteString(text[at:])
	return out.String()
}

// spaceClass matches exactly the runes unicode.IsSpace does, which is
// what stripSpace removes: RE2's \s is ASCII only, and a value holding
// an NBSP or \v would otherwise no longer match its own text (#319).
const spaceClass = `[\t\n\v\f\r \x{85}\p{Z}]*`

// splitPattern matches v with any whitespace between its characters.
func splitPattern(v string) *regexp.Regexp {
	var pattern strings.Builder
	for i, r := range v {
		if i > 0 {
			pattern.WriteString(spaceClass)
		}
		pattern.WriteString(regexp.QuoteMeta(string(r)))
	}
	return regexp.MustCompile(pattern.String())
}

// stripSpace is s without its whitespace.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// CredentialLister is the slice of registry.Store RedactAll reads.
type CredentialLister interface {
	ListCredentials(ctx context.Context) ([]*registry.Credential, error)
}

// ErrUnredactable is RedactAll's error when the vault can't be read: the
// caller must not show the text unscrubbed.
var ErrUnredactable = errors.New("credentials: the vault could not be read to redact the text")

// RedactAll replaces every credential value in the vault — any scope, any
// agent type — found in text, for text leaving Loomux with no particular
// scope in mind. If the vault can't be read it returns ErrUnredactable and
// no text: fail closed.
func RedactAll(ctx context.Context, store CredentialLister, text string) (string, error) {
	creds, err := store.ListCredentials(ctx)
	if err != nil {
		return "", errors.Join(ErrUnredactable, err)
	}
	values := make(map[string]string, len(creds))
	for _, c := range creds {
		values[c.ID] = c.Value
	}
	return RedactValues(text, values), nil
}
