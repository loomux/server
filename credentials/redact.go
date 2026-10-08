package credentials

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Loomux/server/registry"
)

// minRedactLen is the shortest credential value redacted: anything
// shorter would match ordinary text.
const minRedactLen = 6

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
// "[redacted]". The longest values go first (ties in
// byte order, so the result never depends on map order): when one value
// is a prefix or substring of another, replacing the short one first
// would leave the long one's remainder in the text (LOOM-156).
func RedactValues(text string, secrets map[string]string) string {
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= minRedactLen {
			values = append(values, v)
		}
	}
	systemSecrets.mu.RLock()
	for v := range systemSecrets.values {
		values = append(values, v)
	}
	systemSecrets.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	for _, v := range values {
		text = strings.ReplaceAll(text, v, "[redacted]")
	}
	return text
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
