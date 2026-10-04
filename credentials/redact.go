package credentials

import (
	"context"
	"errors"
	"strings"

	"github.com/Loomux/server/registry"
)

// minRedactLen is the shortest credential value redacted: anything
// shorter would match ordinary text.
const minRedactLen = 6

// RedactValues replaces every value in secrets (of at least six bytes)
// found in text with "[redacted]".
func RedactValues(text string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) >= minRedactLen {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
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
