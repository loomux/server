package router_test

import (
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/router"
)

func TestAgentTypeRegistry_CompletionConfig(t *testing.T) {
	reg := router.AgentTypeRegistry{
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker},
			LaunchTemplate: "claude",
		},
		"codex": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 10 * time.Second},
			LaunchTemplate: "codex",
		},
	}

	cfg := reg.CompletionConfig()

	if len(cfg) != 2 {
		t.Fatalf("CompletionConfig() has %d entries, want 2", len(cfg))
	}
	if cfg["claude-code"].Tier != completion.TierMarker {
		t.Fatalf(`CompletionConfig()["claude-code"].Tier = %v, want TierMarker`, cfg["claude-code"].Tier)
	}
	if cfg["codex"].Tier != completion.TierIdle || cfg["codex"].IdleTimeout != 10*time.Second {
		t.Fatalf(`CompletionConfig()["codex"] = %+v, want Tier=TierIdle IdleTimeout=10s`, cfg["codex"])
	}
}

func TestAgentTypeRegistry_LaunchCommand(t *testing.T) {
	reg := router.AgentTypeRegistry{
		"claude-code": router.AgentType{LaunchTemplate: "claude"},
	}

	got, err := reg.LaunchCommand("claude-code")
	if err != nil {
		t.Fatalf("LaunchCommand: %v", err)
	}
	if got != "claude" {
		t.Fatalf("LaunchCommand(claude-code) = %q, want %q", got, "claude")
	}

	if _, err := reg.LaunchCommand("does-not-exist"); err == nil {
		t.Fatalf("LaunchCommand(does-not-exist): got nil error, want an error")
	}
}
