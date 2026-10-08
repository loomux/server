package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
)

func routerTier(tier, key string) *registry.RouterTier {
	return &registry.RouterTier{Tier: tier, Provider: "openai", BaseURL: "https://api.example/v1", Model: "m-" + tier,
		APIKey: key, KeyFingerprint: "sha256:" + tier, KeyLast4: key[len(key)-4:]}
}

func openRouterStore(t *testing.T, path string, key []byte) *Store {
	t.Helper()
	var opts []Option
	if key != nil {
		opts = append(opts, WithMasterKey(key))
	}
	s, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// LOOM-185: a router tier's key round-trips, is encrypted at rest, and
// each ciphertext is bound to its tier: copied onto the other tier's row
// it no longer decrypts.
func TestRouterTier_EncryptedRoundTripAndBoundToItsTier(t *testing.T) {
	ctx := context.Background()
	s := openRouterStore(t, filepath.Join(t.TempDir(), "r.db"), bytes.Repeat([]byte("k"), 32))
	for _, rt := range []*registry.RouterTier{routerTier("primary", "PRIMARY-KEY-123456"), routerTier("escalation", "ESCALATION-KEY-7890")} {
		if err := s.SetRouterTier(ctx, rt, &registry.RouterSettingsChange{Tier: rt.Tier, Action: "set", Fields: []string{"api_key"}, Actor: "session:x"}); err != nil {
			t.Fatal(err)
		}
		if rt.SetAt.IsZero() {
			t.Fatal("SetAt not stamped")
		}
	}
	got, err := s.ListRouterTiers(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListRouterTiers = %v, %v", got, err)
	}
	if got[0].Tier != "escalation" || got[0].APIKey != "ESCALATION-KEY-7890" || got[1].Tier != "primary" || got[1].APIKey != "PRIMARY-KEY-123456" ||
		got[1].Model != "m-primary" || got[1].KeyFingerprint != "sha256:primary" || got[1].KeyLast4 != "3456" {
		t.Fatalf("round trip: %+v %+v", got[0], got[1])
	}

	var blob []byte
	if err := s.db.QueryRowContext(ctx, `SELECT api_key FROM router_settings WHERE tier = 'primary'`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("PRIMARY-KEY")) {
		t.Fatal("api_key stored in plaintext")
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE router_settings SET api_key = ? WHERE tier = 'escalation'`, blob); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListRouterTiers(ctx)
	if err == nil {
		t.Fatal("a ciphertext moved to another tier decrypted")
	}
	if len(got) != 2 || got[0].APIKey != "" || got[1].APIKey != "PRIMARY-KEY-123456" {
		t.Fatalf("after the swap: want escalation unreadable, primary intact: %+v %+v", got[0], got[1])
	}
}

// Replacing a tier keeps one row; every change is audited, newest first,
// with field names only.
func TestRouterTier_ReplaceDeleteAndAudit(t *testing.T) {
	ctx := context.Background()
	s := openRouterStore(t, filepath.Join(t.TempDir(), "r.db"), bytes.Repeat([]byte("k"), 32))
	set := func(key string, fields ...string) {
		t.Helper()
		if err := s.SetRouterTier(ctx, routerTier("primary", key), &registry.RouterSettingsChange{Tier: "primary", Action: "set", Fields: fields, Actor: "session:a"}); err != nil {
			t.Fatal(err)
		}
	}
	set("FIRST-KEY-000000", "provider", "base_url", "model", "api_key")
	set("SECOND-KEY-11111", "api_key")
	got, err := s.ListRouterTiers(ctx)
	if err != nil || len(got) != 1 || got[0].APIKey != "SECOND-KEY-11111" {
		t.Fatalf("after replace: %+v, %v", got, err)
	}
	if err := s.DeleteRouterTier(ctx, "primary", &registry.RouterSettingsChange{Tier: "primary", Action: "clear", Actor: "session:b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRouterTier(ctx, "primary", &registry.RouterSettingsChange{Tier: "primary", Action: "clear", Actor: "session:b"}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	changes, err := s.ListRouterSettingsChanges(ctx, 10)
	if err != nil || len(changes) != 3 {
		t.Fatalf("changes = %v, %v (a failed delete must not be audited)", changes, err)
	}
	if changes[0].Action != "clear" || changes[0].Actor != "session:b" || len(changes[0].Fields) != 0 ||
		changes[1].Action != "set" || len(changes[1].Fields) != 1 || changes[1].Fields[0] != "api_key" ||
		len(changes[2].Fields) != 4 || changes[2].ID == "" || changes[2].CreatedAt.IsZero() {
		t.Fatalf("changes: %+v %+v %+v", changes[0], changes[1], changes[2])
	}
	if limited, _ := s.ListRouterSettingsChanges(ctx, 1); len(limited) != 1 || limited[0].ID != changes[0].ID {
		t.Fatalf("limit 1 = %+v", limited)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO router_settings (tier, provider, base_url, model, api_key, key_fingerprint, key_last4, set_at)
		VALUES ('other', 'openai', 'u', 'm', x'00', '', '', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("a tier other than primary/escalation was stored")
	}
}

// Without a master key nothing is stored, and stored rows come back
// keyless with ErrNoMasterKey; with a different key they don't decrypt.
func TestRouterTier_MasterKeyRequired(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.db")
	none := openRouterStore(t, path, nil)
	if err := none.SetRouterTier(ctx, routerTier("primary", "SOME-KEY-0000000"), nil); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Fatalf("SetRouterTier without a master key = %v", err)
	}
	none.Close()

	withKey := openRouterStore(t, path, bytes.Repeat([]byte("k"), 32))
	if err := withKey.SetRouterTier(ctx, routerTier("primary", "SOME-KEY-0000000"), nil); err != nil {
		t.Fatal(err)
	}
	withKey.Close()

	none = openRouterStore(t, path, nil)
	got, err := none.ListRouterTiers(ctx)
	if !errors.Is(err, registry.ErrNoMasterKey) || len(got) != 1 || got[0].APIKey != "" || got[0].Model != "m-primary" {
		t.Fatalf("without a master key: %+v, %v", got, err)
	}
	none.Close()

	other := openRouterStore(t, path, bytes.Repeat([]byte("o"), 32))
	got, err = other.ListRouterTiers(ctx)
	if err == nil || len(got) != 1 || got[0].APIKey != "" {
		t.Fatalf("with another master key: %+v, %v", got, err)
	}
}
