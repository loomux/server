package router

import (
	"strings"
	"testing"
	"time"
)

func TestIsConfirmation(t *testing.T) {
	install := pendingInstall{kind: pendingInstallAgent, agentType: "codex"}
	for _, m := range []string{"yes", "Yes", " YES! ", "y", "yes please", "ok", "go ahead", "do it",
		"install it", "Yes, install it.", "install codex", "yes install codex"} {
		if !isConfirmation(m, install) {
			t.Errorf("isConfirmation(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "no", "nope", "yes but use claude", "install claude-code", "maybe",
		"sure, after lunch", "yes; rm -rf /", "can you install it on bigbox instead"} {
		if isConfirmation(m, install) {
			t.Errorf("isConfirmation(%q) = true, want false", m)
		}
	}
}

func TestPendingActions_TakeIsOneShotAndExpires(t *testing.T) {
	var p pendingActions
	now := time.Now()
	p.put("c", pendingInstall{agentType: "codex", expires: now.Add(time.Minute)})
	if _, ok := p.take("other", now); ok {
		t.Error("take returned another conversation's offer")
	}
	if _, ok := p.take("c", now); !ok {
		t.Fatal("take didn't return the live offer")
	}
	if _, ok := p.take("c", now); ok {
		t.Error("an offer was returned twice")
	}
	p.put("c", pendingInstall{agentType: "codex", expires: now.Add(time.Minute)})
	if _, ok := p.take("c", now.Add(2*time.Minute)); ok {
		t.Error("an expired offer was returned")
	}
}

func TestQuoteOutput_KeepsTheEndWithinBounds(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("the error at the end")
	got := quoteOutput(b.String())
	if !strings.HasSuffix(got, "the error at the end") || !strings.HasPrefix(got, "[… earlier output truncated]") {
		t.Errorf("quoteOutput kept the wrong part:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n > quotedOutputMaxLines {
		t.Errorf("quoteOutput kept %d lines, want at most %d", n, quotedOutputMaxLines)
	}
	if long := quoteOutput(strings.Repeat("x", 10*quotedOutputMaxBytes)); len(long) > quotedOutputMaxBytes+64 {
		t.Errorf("quoteOutput kept %d bytes, want about %d", len(long), quotedOutputMaxBytes)
	}
	if got := quoteOutput("short\n\n"); got != "short" {
		t.Errorf("quoteOutput(short) = %q", got)
	}
}

func TestRedactValues(t *testing.T) {
	got := redactValues("key sk-abc123 and abc", map[string]string{"K": "sk-abc123", "SHORT": "abc"})
	if got != "key [redacted] and abc" {
		t.Errorf("redactValues = %q", got)
	}
}

func TestIsConfirmation_Clone(t *testing.T) {
	clone := pendingInstall{kind: pendingCloneRemote}
	for _, m := range []string{"yes", "clone it", "Go ahead."} {
		if !isConfirmation(m, clone) {
			t.Errorf("isConfirmation(%q, clone) = false, want true", m)
		}
	}
	if isConfirmation("install it", clone) {
		t.Error(`"install it" confirmed a clone`)
	}
	if isConfirmation("clone it", pendingInstall{kind: pendingInstallAgent, agentType: "codex"}) {
		t.Error(`"clone it" confirmed an install`)
	}
}

func TestIsConfirmation_Command(t *testing.T) {
	cmd := pendingInstall{kind: pendingRunCommand, command: "uptime"}
	for _, m := range []string{"yes", "run it", "Go ahead."} {
		if !isConfirmation(m, cmd) {
			t.Errorf("isConfirmation(%q, command) = false, want true", m)
		}
	}
	// An install phrasing doesn't confirm a command, nor a command
	// phrasing an install.
	if isConfirmation("install it", cmd) {
		t.Error(`"install it" confirmed a command`)
	}
	if isConfirmation("run it", pendingInstall{kind: pendingInstallAgent, agentType: "codex"}) {
		t.Error(`"run it" confirmed an install`)
	}
}

func TestIsVerbatim(t *testing.T) {
	cases := []struct {
		command, message string
		want             bool
	}{
		{"hostname && uptime", "run `hostname && uptime` on jet01", true},
		{"hostname && uptime", "run ` hostname && uptime ` on jet01", true},
		{"df -h", "on jet01:\n```\ndf -h\n```", true},
		{"df -h", "on jet01:\n```sh\ndf -h\n```", true},
		{"hostname && uptime", "run hostname && uptime on jet01", false},
		{"rm -rf /tmp/x", "don't run rm -rf /tmp/x there", false},
		{"hostname && uptime", "run `hostname` then `uptime`", false},
		{"hostname", "run `hostname && uptime`", false},
		{"", "``", false},
	}
	for _, tc := range cases {
		if got := isVerbatim(tc.command, tc.message); got != tc.want {
			t.Errorf("isVerbatim(%q, %q) = %v, want %v", tc.command, tc.message, got, tc.want)
		}
	}
}
