package router

import (
	"strings"
	"testing"

	"github.com/Loomux/server/credentials"
)

// LOOM-133: a secret straddling the relay bound is scrubbed whole, before
// the bound cuts it into a piece no pattern recognises.
func TestRelayTextScrubsBeforeBounding(t *testing.T) {
	const token = "ghp_1234567890abcdefghijABCDEFGHIJ123456"
	captured := strings.Repeat("x", MaxRelayInput-10) + " " + token + " tail"
	out := relayText(captured, credentials.RedactPatterns)
	if strings.Contains(out, "ghp_") {
		t.Errorf("a piece of the token reached the relay input: …%s", out[len(out)-80:])
	}
}
