package agents_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/router"
)

// runShell runs script under sh with the given extra args ($0, $1, ...)
// and LOOMUX_MARKER_PATH set to markerPath ("" means unset), the same
// way the agent CLI would run its hook.
func runShell(t *testing.T, markerPath string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	if markerPath != "" {
		cmd.Env = append(cmd.Env, "LOOMUX_MARKER_PATH="+markerPath)
	} else {
		var env []string
		for _, kv := range cmd.Env {
			if !strings.HasPrefix(kv, "LOOMUX_MARKER_PATH=") {
				env = append(env, kv)
			}
		}
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// claudeStopHookCommand extracts the single Stop hook command from the
// --settings JSON ClaudeCode injects, failing the test if the JSON isn't
// the shape Claude Code reads hooks from.
func claudeStopHookCommand(t *testing.T, at router.AgentType) string {
	t.Helper()
	args := at.CompletionHookArgs
	if len(args) != 2 || args[0] != "--settings" {
		t.Fatalf("CompletionHookArgs = %q, want [--settings <json>]", args)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	dec := json.NewDecoder(strings.NewReader(args[1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		t.Fatalf("--settings value is not the expected hooks JSON: %v\n%s", err, args[1])
	}
	if len(settings.Hooks) != 1 || len(settings.Hooks["Stop"]) != 1 || len(settings.Hooks["Stop"][0].Hooks) != 1 {
		t.Fatalf("want exactly one Stop hook and no other events, got %+v", settings.Hooks)
	}
	h := settings.Hooks["Stop"][0].Hooks[0]
	if h.Type != "command" {
		t.Fatalf("Stop hook type = %q, want command", h.Type)
	}
	return h.Command
}

func TestClaudeCode_StopHookTouchesMarker(t *testing.T) {
	command := claudeStopHookCommand(t, agents.ClaudeCode())
	// The marker directory doesn't exist on a fresh target — the hook
	// must create it rather than rely on anything provisioned beforehand.
	marker := filepath.Join(t.TempDir(), "not", "yet", "there", "task.done")
	runShell(t, marker, "sh", "-c", command)
	if !fileExists(marker) {
		t.Fatalf("Stop hook did not create %s", marker)
	}
}

func TestClaudeCode_StopHookIsNoOpWithoutMarkerPath(t *testing.T) {
	command := claudeStopHookCommand(t, agents.ClaudeCode())
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LOOMUX_MARKER_PATH=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook must succeed silently when LOOMUX_MARKER_PATH is unset: %v\n%s", err, out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("hook wrote %v with LOOMUX_MARKER_PATH unset", entries)
	}
}

// codexNotifyArgv returns the notify program argv ClaudeCode's sibling
// Codex adapter configures, checking the -c override has the form Codex
// parses (`notify=<TOML array of strings>`).
func codexNotifyArgv(t *testing.T, at router.AgentType) []string {
	t.Helper()
	args := at.CompletionHookArgs
	if len(args) != 2 || args[0] != "-c" || !strings.HasPrefix(args[1], "notify=") {
		t.Fatalf("CompletionHookArgs = %q, want [-c notify=<array>]", args)
	}
	// Every element is a TOML basic string, and a TOML basic string with
	// only \" and \\ escapes reads the same as a JSON string — so the
	// array can be checked with encoding/json.
	var argv []string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(args[1], "notify=")), &argv); err != nil {
		t.Fatalf("notify value is not an array of strings: %v\n%s", err, args[1])
	}
	if len(argv) < 3 {
		t.Fatalf("notify argv = %q, too short", argv)
	}
	return argv
}

func TestCodex_NotifyTouchesMarkerOnTurnComplete(t *testing.T) {
	argv := codexNotifyArgv(t, agents.Codex())
	marker := filepath.Join(t.TempDir(), "missing-dir", "task.done")
	// Codex appends the event as one JSON argument.
	payload := `{"type":"agent-turn-complete","turn-id":"1","last-assistant-message":"done"}`
	runShell(t, marker, append(argv, payload)...)
	if !fileExists(marker) {
		t.Fatalf("notify did not create %s on agent-turn-complete", marker)
	}
}

func TestCodex_NotifyIgnoresOtherEvents(t *testing.T) {
	argv := codexNotifyArgv(t, agents.Codex())
	marker := filepath.Join(t.TempDir(), "task.done")
	runShell(t, marker, append(argv, `{"type":"approval-requested"}`)...)
	if fileExists(marker) {
		t.Fatal("notify touched the marker for an event that isn't a finished turn")
	}
}

func TestAdapters_DeclareMarkerTierAndVersionFloor(t *testing.T) {
	for name, at := range map[string]router.AgentType{"claude-code": agents.ClaudeCode(), "codex": agents.Codex()} {
		t.Run(name, func(t *testing.T) {
			if at.Tier != completion.TierMarker {
				t.Errorf("Tier = %v, want TierMarker", at.Tier)
			}
			if at.VersionCheck == nil || at.VersionCheck.Min == "" {
				t.Fatalf("VersionCheck = %+v, want a minimum version (the completion hook flag must exist)", at.VersionCheck)
			}
			if at.VersionCheck.Requires == "" {
				t.Error("VersionCheck.Requires is empty; a too-old CLI must say what it's too old for")
			}
			// Binary must equal the template's binary: router swaps it for
			// the probed absolute path in both the launch and the version
			// check (LOOM-79). Dropping it would silently stop probing.
			if at.Binary == "" || at.Binary != at.LaunchTemplate {
				t.Errorf("Binary = %q, want %q (probed, then launched by absolute path)", at.Binary, at.LaunchTemplate)
			}
			if at.Install == nil || at.Install.Command == "" || at.Install.Login == "" {
				t.Errorf("Install = %+v, want an install recipe and login step (LOOM-71)", at.Install)
			}
			if !strings.HasPrefix(at.VersionCheck.Command, at.LaunchTemplate+" ") {
				t.Errorf("VersionCheck.Command = %q, want it to run the launched binary %q", at.VersionCheck.Command, at.LaunchTemplate)
			}
		})
	}
}

// The floors are the oldest releases whose completion hook was verified
// with a real turn — not the permissive 1.0.0/0.100.0 LOOM-79 started
// with, which would admit CLIs that may lack the hook flags.
func TestAdapters_VersionFloorsAreTheVerifiedOnes(t *testing.T) {
	for name, c := range map[string]struct {
		at          router.AgentType
		min, tooOld string
	}{
		"claude-code": {agents.ClaudeCode(), "2.1.0", "2.0.99 (Claude Code)"},
		"codex":       {agents.Codex(), "0.150.0", "codex-cli 0.149.9"},
	} {
		vc := c.at.VersionCheck
		if vc.Min != c.min {
			t.Errorf("%s Min = %q, want %q", name, vc.Min, c.min)
		}
		v, err := vc.Parse(c.tooOld)
		if err != nil {
			t.Fatalf("%s Parse(%q): %v", name, c.tooOld, err)
		}
		if err := router.CheckVersionRange(v, vc.Min, vc.Max); err == nil {
			t.Errorf("%s accepted %q, below the verified floor", name, c.tooOld)
		}
	}
}
