package router

import (
	"strings"
	"testing"
	"time"
)

func TestIsConfirmation(t *testing.T) {
	for _, m := range []string{"yes", "Yes", " YES! ", "y", "yes please", "ok", "go ahead", "do it",
		"install it", "Yes, install it.", "install codex", "yes install codex"} {
		if !isConfirmation(m, "codex") {
			t.Errorf("isConfirmation(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "no", "nope", "yes but use claude", "install claude-code", "maybe",
		"sure, after lunch", "yes; rm -rf /", "can you install it on bigbox instead"} {
		if isConfirmation(m, "codex") {
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
