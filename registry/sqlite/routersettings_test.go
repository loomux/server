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

// LOOM-189: a tier's key is bound to the provider and base URL it was
// set for as well: a row whose provider or base_url is changed behind
// the store's back (to send the key elsewhere) no longer decrypts.
func TestRouterTier_BoundToProviderAndBaseURL(t *testing.T) {
	ctx := context.Background()
	s := openRouterStore(t, filepath.Join(t.TempDir(), "r.db"), bytes.Repeat([]byte("k"), 32))
	for _, change := range []string{
		`UPDATE router_settings SET base_url = 'https://collector.example/v1'`,
		`UPDATE router_settings SET provider = 'anthropic'`,
	} {
		if err := s.SetRouterTier(ctx, routerTier("primary", "PRIMARY-KEY-123456"), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, change); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListRouterTiers(ctx)
		if err == nil || len(got) != 1 || got[0].APIKey != "" {
			t.Errorf("after %q: ListRouterTiers = %+v, %v; want the key unreadable", change, got, err)
		}
	}
}

// LOOM-189: a tier sealed before the binding (to its tier alone) is
// re-sealed to its tier, provider and base URL by the first Open with
// the master key, which records the re-seal as done. Without the key, or
// with another, it is left alone and nothing is recorded. A row sealed
// the old way that appears after that isn't read, nor re-sealed.
func TestRouterTier_ResealOnOpen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "r.db")
	key := bytes.Repeat([]byte{7}, 32)
	count := func(s *Store, query string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const resealedRows = `SELECT COUNT(*) FROM router_settings WHERE seal_version = 2`
	const resealDone = `SELECT COUNT(*) FROM router_settings_reseal WHERE seal_version = 2`
	insertV1 := func(s *Store, tier, apiKey string) {
		t.Helper()
		v1, err := seal(key, []byte(apiKey), []byte("router_tier:"+tier))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO router_settings (tier, provider, base_url, model, api_key, key_fingerprint, key_last4, set_at, seal_version)
			VALUES (?, 'openai', 'https://api.example/v1', 'm', ?, 'sha256:x', 'last', CURRENT_TIMESTAMP, 1)
			ON CONFLICT (tier) DO UPDATE SET api_key = excluded.api_key, seal_version = 1`, tier, v1); err != nil {
			t.Fatal(err)
		}
	}

	for i, opts := range [][]Option{nil, {WithMasterKey(bytes.Repeat([]byte{9}, 32))}} {
		s, err := Open(dbPath, opts...)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if i == 0 {
			insertV1(s, "primary", "LOOM-185-PRIMARY-KEY")
		}
		if n := count(s, resealedRows); n != 0 {
			t.Errorf("%d rows re-sealed without the right key; want none", n)
		}
		if n := count(s, resealDone); n != 0 {
			t.Error("re-seal recorded as done without the right key")
		}
		s.Close()
	}

	s, err := Open(dbPath, WithMasterKey(key))
	if err != nil {
		t.Fatalf("Open with the key: %v", err)
	}
	if n := count(s, resealedRows); n != 1 {
		t.Errorf("%d rows re-sealed; want 1", n)
	}
	if n := count(s, resealDone); n != 1 {
		t.Error("re-seal not recorded as done")
	}
	got, err := s.ListRouterTiers(ctx)
	if err != nil || len(got) != 1 || got[0].APIKey != "LOOM-185-PRIMARY-KEY" {
		t.Fatalf("after the re-seal: %+v, %v", got, err)
	}
	insertV1(s, "primary", "LATE-OLD-WAY-KEY")
	if got, err := s.ListRouterTiers(ctx); err == nil || got[0].APIKey != "" {
		t.Errorf("a row sealed the old way read as %+v; want an error", got)
	}
	s.Close()

	s, err = Open(dbPath, WithMasterKey(key))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if n := count(s, resealedRows); n != 0 {
		t.Errorf("%d rows at seal_version 2 after a reopen; want 0 (no re-seal once done)", n)
	}
}
