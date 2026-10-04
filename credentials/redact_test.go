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
