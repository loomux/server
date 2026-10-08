package router

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Loomux/server/targets"
)

// A health probe that couldn't connect says why in plain words, with the
// SSH failure's hint (LOOM-85), not ssh's raw stderr.
func TestProbeFailureReason_SSHHint(t *testing.T) {
	err := fmt.Errorf("probe: %w", &targets.UnreachableError{Host: "jet01", Failure: targets.SSHAuthFailed, Detail: "loomux@jet01: Permission denied (publickey)."})
	got := probeFailureReason(err)
	if !strings.HasPrefix(got, "unreachable: jet01 refused Loomux's SSH key") || strings.Contains(got, "Permission denied") {
		t.Errorf("probeFailureReason = %q", got)
	}
	other := &targets.UnreachableError{Host: "jet01", Failure: targets.SSHOther, Detail: "something odd"}
	if got := probeFailureReason(other); got != "unreachable: could not connect to jet01: something odd (other)" {
		t.Errorf("probeFailureReason(other) = %q", got)
	}
}

// The failure's class ends the reason (#294 review), so what reads a
// target's health — onboarding's next step, test's steps — can tell an
// unauthorized key from a host key problem from no route at all.
func TestProbeFailureReason_KeepsTheClass(t *testing.T) {
	for _, f := range []targets.SSHFailure{targets.SSHAuthFailed, targets.SSHHostKeyChanged, targets.SSHHostKeyUnknown, targets.SSHProxyUnreachable} {
		got := probeFailureReason(&targets.UnreachableError{Host: "jet01", Failure: f, Detail: "raw ssh text", Managed: true})
		if !strings.HasSuffix(got, " ("+string(f)+")") || strings.Contains(got, "raw ssh text") {
			t.Errorf("probeFailureReason(%s) = %q", f, got)
		}
	}
}
