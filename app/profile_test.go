package app

import (
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

func TestLoadConfig_AgentProfilesFromEnv(t *testing.T) {
	setRouterEnv(t)
	t.Setenv(envAgentProfiles, `{"claude-code":{"permission_args":["--permission-mode","plan"],"prompt_as_arg":false},"codex":{"pre_trust":false}}`)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cc := cfg.AgentProfiles["claude-code"]
	if cc.PermissionArgs == nil || strings.Join(*cc.PermissionArgs, " ") != "--permission-mode plan" {
		t.Errorf("claude-code permission_args = %v", cc.PermissionArgs)
	}
	if cc.PromptAsArg == nil || *cc.PromptAsArg {
		t.Errorf("claude-code prompt_as_arg = %v, want false", cc.PromptAsArg)
	}
	if cx := cfg.AgentProfiles["codex"]; cx.PreTrust == nil || *cx.PreTrust {
		t.Errorf("codex pre_trust = %v, want false", cx.PreTrust)
	}
}

func TestLoadConfig_AgentProfilesUnsetMeansDefaults(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.AgentProfiles) != 0 {
		t.Errorf("AgentProfiles = %v, want none", cfg.AgentProfiles)
	}
}

func TestLoadConfig_AgentProfilesMalformed(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      `{`,
		"unknown field": `{"claude-code":{"permision_args":["--x"]}}`, // typo must not be ignored
	} {
		t.Run(name, func(t *testing.T) {
			setRouterEnv(t)
			t.Setenv(envAgentProfiles, raw)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig: want error, got nil")
			}
		})
	}
}

func TestBuild_AgentProfileForUnknownAgentType_Fails(t *testing.T) {
	cfg := testConfig(t, "http://example.invalid")
	cfg.AgentProfiles = map[string]router.ProfileOverride{"gemini": {}}
	if _, err := Build(cfg); err == nil {
		t.Fatal("Build: want error for a profile override naming no registered agent type")
	}
}
