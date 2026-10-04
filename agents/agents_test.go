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
	"github.com/Loomux/server/registry"
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

// TestClaudeCode_Profile pins the unattended defaults: auto permission
// mode (classifier-gated; not bypass), named modes a target can pick,
// the workspace pre-trusted before launch, interruptible, and the first
// prompt as an argument.
func TestClaudeCode_Profile(t *testing.T) {
	at := agents.ClaudeCode()
	p := at.Profile
	if got := strings.Join(p.PermissionArgs, " "); got != "--permission-mode auto" {
		t.Errorf("PermissionArgs = %q, want auto mode", got)
	}
	for mode, want := range map[string]string{
		"auto": "--permission-mode auto", "accept-edits": "--permission-mode acceptEdits", "manual": "--permission-mode manual",
	} {
		if got := strings.Join(p.PermissionModes[mode], " "); got != want {
			t.Errorf("PermissionModes[%q] = %q, want %q", mode, got, want)
		}
	}
	for _, args := range p.PermissionModes {
		for _, a := range args {
			if strings.Contains(a, "bypass") || strings.Contains(a, "dangerously") {
				t.Errorf("a permission mode bypasses permissions: %q", args)
			}
		}
	}
	if p.TrustCommand == nil {
		t.Error("TrustCommand nil: every new workspace would stop at Claude's folder-trust dialog")
	}
	if strings.Join(at.InterruptKeys, ",") != "Escape" {
		t.Errorf("InterruptKeys = %q, want Escape (LOOM-117)", at.InterruptKeys)
	}
	if !p.PromptAsArg {
		t.Error("PromptAsArg = false, want the first prompt passed as an argument")
	}
}

// The trust command marks exactly that workspace path trusted in
// ~/.claude.json, keeping everything else in the file — never a global
// trust. Run for real under sh with a scratch HOME.
func TestClaudeCode_TrustCommandMarksOnlyThatPath(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	home := t.TempDir()
	ws := filepath.Join(home, "loomux-workspaces", `my "ws" $x`)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "other")
	before := map[string]any{
		"numStartups": 7,
		"projects":    map[string]any{other: map[string]any{"hasTrustDialogAccepted": false, "allowedTools": []any{"x"}}},
	}
	raw, _ := json.Marshal(before)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", agents.ClaudeCode().Profile.TrustCommand(ws))
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("trust command: %v\n%s", err, out)
	}
	var after map[string]any
	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("~/.claude.json no longer JSON: %v", err)
	}
	projects := after["projects"].(map[string]any)
	if got := projects[ws].(map[string]any)["hasTrustDialogAccepted"]; got != true {
		t.Errorf("workspace trust = %v, want true", got)
	}
	if got := projects[other].(map[string]any); got["hasTrustDialogAccepted"] != false || got["allowedTools"] == nil {
		t.Errorf("another project was changed: %v", got)
	}
	if after["numStartups"] != float64(7) || len(projects) != 2 {
		t.Errorf("rest of the file changed: %v", after)
	}
	if info, _ := os.Stat(filepath.Join(home, ".claude.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600 kept", info.Mode().Perm())
	}

	// No ~/.claude.json yet: created with just that trust.
	home2 := t.TempDir()
	cmd = exec.Command("sh", "-c", agents.ClaudeCode().Profile.TrustCommand(ws))
	cmd.Env = append(os.Environ(), "HOME="+home2)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("trust command (no file): %v\n%s", err, out)
	}
	data, _ = os.ReadFile(filepath.Join(home2, ".claude.json"))
	if !strings.Contains(string(data), "hasTrustDialogAccepted") {
		t.Errorf("new ~/.claude.json = %s", data)
	}
}

func TestCodex_Profile(t *testing.T) {
	p := agents.Codex().Profile
	if got := strings.Join(p.PermissionArgs, " "); got != "--ask-for-approval on-request --sandbox workspace-write" {
		t.Errorf("PermissionArgs = %q", got)
	}
	if !p.PromptAsArg {
		t.Error("PromptAsArg = false, want the first prompt passed as an argument")
	}
	if p.TrustArgs == nil {
		t.Fatal("TrustArgs nil: codex stops at its trust screen without a per-launch projects override")
	}
	// A path with a dot, a quote and a backslash: the dotted -c key form
	// (projects."<dir>".trust_level) splits on the dot, so the override
	// must be an inline table with the path as a quoted TOML key.
	args := p.TrustArgs(`/srv/ws/my.repo "x"\y`)
	want := []string{"-c", `projects={"/srv/ws/my.repo \"x\"\\y"={trust_level="trusted"}}`}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("TrustArgs = %q, want %q", args, want)
	}
}

func TestCodex_TrustArgsEscapeControlCharacters(t *testing.T) {
	args := agents.Codex().Profile.TrustArgs("/ws/a\nb\tc")
	want := `projects={"/ws/a\u000Ab\u0009c"={trust_level="trusted"}}`
	if len(args) != 2 || args[1] != want {
		t.Fatalf("TrustArgs = %q, want [-c %s]", args, want)
	}
}

// LOOM-88: each agent type describes itself for the routing prompt, by
// the name a user would call it.
func TestAgentDescriptions(t *testing.T) {
	for name, at := range map[string]router.AgentType{"claude": agents.ClaudeCode(), "codex": agents.Codex()} {
		if !strings.Contains(strings.ToLower(at.Description), name) {
			t.Errorf("%s description = %q, want it to name %q", name, at.Description, name)
		}
	}
}

// LOOM-86: each adapter's auth check reads what its CLI actually prints.
func TestAdapters_AuthCheckClassifiesRealOutput(t *testing.T) {
	cases := []struct {
		agent  router.AgentType
		output string
		want   string
	}{
		{agents.ClaudeCode(), "{\n  \"loggedIn\": true,\n  \"authMethod\": \"claude.ai\"\n}", registry.AgentAuthLoggedIn},
		{agents.ClaudeCode(), "{\n  \"loggedIn\": false\n}", registry.AgentAuthLoggedOut},
		{agents.ClaudeCode(), "error: unknown command 'auth'", registry.AgentAuthUnknown},
		{agents.Codex(), "Logged in using ChatGPT", registry.AgentAuthLoggedIn},
		{agents.Codex(), "Not logged in", registry.AgentAuthLoggedOut},
	}
	for _, tc := range cases {
		if tc.agent.AuthCheck == nil {
			t.Fatalf("%s has no auth check", tc.agent.LaunchTemplate)
		}
		if got := tc.agent.AuthCheck.Status(tc.output); got != tc.want {
			t.Errorf("%s auth check on %q = %q, want %q", tc.agent.LaunchTemplate, tc.output, got, tc.want)
		}
	}
}
