package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runClaim(t *testing.T, marker string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", claimScript(marker, claimedMarker(marker))).CombinedOutput()
	if err != nil {
		t.Fatalf("claim script: %v: %s", err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// The late-output claim (LOOM-121 review) moves a marker and its payload
// aside, keeps an earlier claim over a newer marker, and claims nothing
// when there is nothing.
func TestClaimScript(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "t1.done")
	c := claimedMarker(m)

	if out := runClaim(t, m); out != "" {
		t.Fatalf("nothing to claim: output %q", out)
	}

	write(t, m, "")
	write(t, m+".reply", "first")
	if out := runClaim(t, m); out != "claimed\n" {
		t.Fatalf("output %q, want claimed", out)
	}
	if _, ok := read(t, m); ok {
		t.Fatalf("marker left in place after the claim")
	}
	if got, _ := read(t, c+".reply"); got != "first" {
		t.Fatalf("claimed payload = %q, want first", got)
	}

	// The agent writes again while the first claim is relayed (or after
	// its relay failed): the newer files stay put, the claim stays.
	write(t, m, "")
	write(t, m+".reply", "second")
	if out := runClaim(t, m); out != "claimed\n" {
		t.Fatalf("output %q, want the earlier claim", out)
	}
	if got, _ := read(t, c+".reply"); got != "first" {
		t.Fatalf("claimed payload = %q, want the earlier claim kept", got)
	}
	if got, _ := read(t, m+".reply"); got != "second" {
		t.Fatalf("newer payload = %q, want it untouched", got)
	}

	// The first one relayed and removed: the next pass claims the second.
	os.Remove(c)
	os.Remove(c + ".reply")
	runClaim(t, m)
	if got, _ := read(t, c+".reply"); got != "second" {
		t.Fatalf("claimed payload = %q, want second", got)
	}

	// A marker with no payload drops a stale claimed payload.
	os.Remove(c)
	write(t, m, "")
	runClaim(t, m)
	if _, ok := read(t, c+".reply"); ok {
		t.Fatalf("stale claimed payload kept for a marker without one")
	}
}
