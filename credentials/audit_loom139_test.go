package credentials_test

// LOOM-139 audit regressions.

import (
	"strings"
	"testing"

	"github.com/Loomux/server/credentials"
)

// When one vault value is a prefix of another, replacing the short one
// first leaves the long one's tail in the text. Map order is random, so
// the leak is intermittent; 200 rounds make it near-certain.
func TestAudit_RedactValues_OverlappingValuesNeverLeak(t *testing.T) {
	secrets := map[string]string{"short": "abcdef", "long": "abcdefXYZ123456"}
	for i := 0; i < 200; i++ {
		if got := credentials.RedactValues("token=abcdefXYZ123456", secrets); strings.Contains(got, "XYZ") {
			t.Fatalf("round %d: %q leaks part of the long value", i, got)
		}
	}
}
