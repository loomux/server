package router_test

import (
	"strings"
	"testing"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/registry"
)

// LOOM-260: every Codex launch turns off the CLI's startup update check,
// so a pinned CLI can't stop the session at its "Update available" menu
// (whose option runs `curl … | sh`). The key is top-level and its value
// is TOML: Codex 0.162.0 documents it in
// codex-rs/config/src/config_toml.rs, and -c parses the value as TOML
// (codex-rs/utils/cli/src/config_override.rs), so `false` is a boolean,
// not the string "false".
func TestCodexLaunchCommandDisablesStartupUpdateCheck(t *testing.T) {
	got := agents.Codex().LaunchCommandForTest("/srv/ws", "do the work", registry.PermissionModeAuto)

	const override = "'-c' 'check_for_update_on_startup=false'"
	if !strings.Contains(got, override) {
		t.Fatalf("launch command %q does not contain %s", got, override)
	}
	if n := strings.Count(got, "check_for_update_on_startup"); n != 1 {
		t.Errorf("launch command has %d update-check overrides, want 1: %q", n, got)
	}
	// The override is its own argument, before the first prompt, so it
	// can't be read as part of the binary or the message.
	if !strings.HasSuffix(got, "'--' 'do the work'") {
		t.Errorf("launch command %q does not end with the first prompt", got)
	}
}
