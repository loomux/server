package router_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// TestIntegration_ProbeResolvesAgentOffThePath runs the real probe
// through a real shell (LOOM-79). The sc1 case: claude installed only in
// ~/.local/bin, which a non-interactive shell's PATH doesn't include — and
// the login-shell case, a CLI on a PATH only a login shell's profile sets
// up. Both must resolve to an absolute path with the version it reports.
func TestIntegration_ProbeResolvesAgentOffThePath(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) string // returns the expected path
	}{
		{"known dir (~/.local/bin)", func(t *testing.T, home string) string {
			return writeFakeAgent(t, filepath.Join(home, ".local", "bin"))
		}},
		{"login-shell PATH only", func(t *testing.T, home string) string {
			dir := filepath.Join(home, "tools", "bin")
			path := writeFakeAgent(t, dir)
			if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("PATH=\""+dir+":$PATH\"\nexport PATH\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "/bin/sh")
			want := tc.setup(t, home)

			store := newTestStore(t)
			target := &registry.Target{ID: uuid.NewString(), Name: "local", Kind: registry.TargetKindLocal}
			if err := store.CreateTarget(context.Background(), target); err != nil {
				t.Fatalf("CreateTarget: %v", err)
			}
			agentTypes := router.AgentTypeRegistry{
				"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: time.Second}},
				"fake": router.AgentType{
					AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle},
					LaunchTemplate: "loomux-fake-agent", Binary: "loomux-fake-agent",
				},
				"missing": router.AgentType{
					AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle},
					LaunchTemplate: "loomux-definitely-not-installed", Binary: "loomux-definitely-not-installed",
				},
			}
			detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), t.TempDir())
			orch := orchestrator.New(store, targets.NewExecutor, detector)
			r := router.New(store, orch, targets.NewExecutor, credentials.NewResolver(store), agentTypes,
				&routertest.StubRoutingModel{}, t.TempDir())

			got, err := r.RefreshTargetAgents(context.Background(), target.ID)
			if err != nil {
				t.Fatalf("RefreshTargetAgents: %v", err)
			}
			byName := map[string]*registry.TargetAgent{}
			for _, a := range got {
				byName[a.AgentType] = a
			}
			fake := byName["fake"]
			if fake == nil || !fake.Available || fake.Path != want || fake.Version != "loomux-fake-agent 4.5.6" {
				t.Errorf("fake agent = %+v, want available at %s reporting version 4.5.6", fake, want)
			}
			if m := byName["missing"]; m == nil || m.Available || m.Path != "" {
				t.Errorf("missing agent = %+v, want unavailable with no path", m)
			}
		})
	}
}

func writeFakeAgent(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "loomux-fake-agent")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'loomux-fake-agent 4.5.6'; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
