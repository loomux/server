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
