package credentials_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Loomux/server/credentials"
)

// TestShellEnvPrefix_RoundTripsThroughRealShell builds a prefix for a
// deliberately nasty value (quotes, semicolons, a command substitution
// attempt) and actually executes it via a real shell, asserting the
// original value comes back byte-for-byte. This is a stronger guarantee
// than asserting on the escaped string's exact form: it proves the
// output is genuinely safe to hand to a real shell, not just that it
// looks plausible.
//
// The launched command is `printenv`, not e.g. `printf "$MY_SECRET"` on
// the same line — a shell prefix assignment ("VAR=value cmd") only sets
// VAR in the environment of the newly exec'd process; the classic
// gotcha is that "$VAR" inside that same command's own argument list is
// expanded by the *parent* shell first, before the assignment applies,
// so it comes out empty. printenv reads its own process environment
// directly, matching how a real launch command (a new process) would
// actually see these values.
func TestShellEnvPrefix_RoundTripsThroughRealShell(t *testing.T) {
	nasty := `it's a "tricky" value; rm -rf / #$(whoami)` + "`echo pwned`"
	secrets := map[string]string{"MY_SECRET": nasty}

	prefix, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		t.Fatalf("ShellEnvPrefix: %v", err)
	}

	cmd := exec.Command("sh", "-c", prefix+`printenv MY_SECRET`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the produced prefix: %v", err)
	}
	got := strings.TrimSuffix(string(out), "\n")
	if got != nasty {
		t.Fatalf("round-tripped value = %q, want %q", got, nasty)
	}
}

func TestShellEnvPrefix_MultipleVarsAllPresent(t *testing.T) {
	secrets := map[string]string{"VAR_A": "value-a", "VAR_B": "value-b"}
	prefix, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		t.Fatalf("ShellEnvPrefix: %v", err)
	}

	// A nested `sh -c` is a genuinely separate process (like a real
	// launch command would be), so its own "$VAR_A"/"$VAR_B" expansion
	// correctly sees the inherited environment — avoiding the same
	// timing gotcha described above.
	cmd := exec.Command("sh", "-c", prefix+`sh -c 'printf "%s|%s" "$VAR_A" "$VAR_B"'`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the produced prefix: %v", err)
	}
	if string(out) != "value-a|value-b" {
		t.Fatalf("got %q, want %q", string(out), "value-a|value-b")
	}
}

func TestShellEnvPrefix_RejectsInvalidNames(t *testing.T) {
	cases := []string{"1BAD", "BAD-NAME", "BAD NAME", "BAD;rm -rf", ""}
	for _, name := range cases {
		_, err := credentials.ShellEnvPrefix(map[string]string{name: "value"})
		if err == nil {
			t.Fatalf("ShellEnvPrefix with invalid name %q: got nil error, want a rejection", name)
		}
	}
}

func TestShellEnvPrefix_Deterministic(t *testing.T) {
	secrets := map[string]string{"Z_VAR": "z", "A_VAR": "a", "M_VAR": "m"}
	first, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		t.Fatalf("ShellEnvPrefix: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := credentials.ShellEnvPrefix(secrets)
		if err != nil {
			t.Fatalf("ShellEnvPrefix: %v", err)
		}
		if got != first {
			t.Fatalf("ShellEnvPrefix output not deterministic: %q vs %q", got, first)
		}
	}
	if !strings.HasPrefix(first, "A_VAR=") {
		t.Fatalf("ShellEnvPrefix = %q, want sorted output starting with A_VAR", first)
	}
}

func TestShellEnvPrefix_Empty(t *testing.T) {
	got, err := credentials.ShellEnvPrefix(map[string]string{})
	if err != nil {
		t.Fatalf("ShellEnvPrefix: %v", err)
	}
	if got != "" {
		t.Fatalf("ShellEnvPrefix(empty) = %q, want empty string", got)
	}
}
