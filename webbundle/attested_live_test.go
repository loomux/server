package webbundle

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAttestedLive checks the real thing: loomux/web's newest release on
// GitHub, its attestation verified against Sigstore's public-good root
// (fetched over TUF). It needs the network, so it only runs with
// LOOMUX_LIVE=1: go test ./webbundle -run Live -v
func TestAttestedLive(t *testing.T) {
	if os.Getenv("LOOMUX_LIVE") == "" {
		t.Skip("set LOOMUX_LIVE=1 to verify against GitHub and Sigstore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := NewAttested("loomux/web", os.Getenv("GITHUB_TOKEN"), t.TempDir())
	rel, err := a.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	t.Logf("newest attested release: %s (commit %s, sha256 %s)", rel.Tag, rel.Commit, rel.SHA256)

	m, err := New(bakedDir(t, nil), t.TempDir(), a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Install(ctx)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Logf("installed %s into %s", got.Tag, m.Root())
}
