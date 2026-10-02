package router_test

import (
	"context"
	"strings"
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

// TestIntegration_ProcessExit replays the 2026-10-02 incident (LOOM-71,
// LOOM-74) against real tmux: a provisioning command that exits at once
// used to take its session with it and fail as "can't find pane", leaving
// the workspace active. Now exit 0 completes provisioning, a non-zero
// exit fails it with the script's output and a 'failed' workspace, and an
// agent CLI that isn't on PATH fails the turn in the shell's own words.
// No loomux session is left behind by a successful provisioning.
func TestIntegration_ProcessExit(t *testing.T) {
	cases := []struct {
		name          string
		provision     router.ProvisionKind
		agentLaunch   string
		wantErr       []string
		wantWSStatus  registry.WorkspaceStatus
		wantProvTasks registry.TaskStatus
	}{
		{name: "provision exits 0", provision: router.ProvisionEmpty, agentLaunch: `sh -c 'echo agent-output; sleep 30'`,
			wantWSStatus: registry.WorkspaceStatusIdle, wantProvTasks: registry.TaskStatusCompleted},
		// existing_dir of a directory that isn't there: the recipe exits 1.
		{name: "provision exits non-zero", provision: router.ProvisionExistingDir,
			wantErr: []string{"status 1", "no directory"}, wantWSStatus: registry.WorkspaceStatusFailed,
			wantProvTasks: registry.TaskStatusFailed},
		{name: "agent not installed", provision: router.ProvisionEmpty, agentLaunch: "loomux-no-such-agent-cli",
			wantErr:      []string{"status 127", "loomux-no-such-agent-cli"},
			wantWSStatus: registry.WorkspaceStatusIdle, wantProvTasks: registry.TaskStatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			target := &registry.Target{ID: uuid.NewString(), Name: "local", Kind: registry.TargetKindLocal, WorkspaceRoot: t.TempDir()}
			if err := store.CreateTarget(ctx, target); err != nil {
				t.Fatalf("CreateTarget: %v", err)
			}
			agentTypes := router.AgentTypeRegistry{
				"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 10 * time.Second}},
				"agent": router.AgentType{
					AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 500 * time.Millisecond},
					LaunchTemplate: tc.agentLaunch,
				},
			}
			markerDir := t.TempDir()
			detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
			orch := orchestrator.New(store, targets.NewExecutor, detector)
			model := &routertest.StubRoutingModel{
				DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
					return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "agent", NewWorkspace: router.ProvisionSpec{
						Name: "ws", TargetID: target.ID, Kind: tc.provision,
					}}, nil
				},
				RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
					return router.RelayResult{Reply: strings.TrimSpace(captured), Done: true}, nil
				},
			}
			r := router.New(store, orch, targets.NewExecutor, credentials.NewResolver(store), agentTypes, model, markerDir)
			exec := targets.NewLocalExecutor()
			t.Cleanup(func() { killWorkspaceSessions(t, store, exec) })

			start := time.Now()
			reply, err := r.Dispatch(ctx, "conv", "go")
			if elapsed := time.Since(start); elapsed > 8*time.Second {
				t.Errorf("Dispatch took %s; a process exit should end the wait at once, not an idle timeout", elapsed)
			}
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Dispatch: %v", err)
				}
				if !strings.Contains(reply, "agent-output") {
					t.Errorf("reply = %q, want the agent's output", reply)
				}
			} else {
				if err == nil {
					t.Fatalf("Dispatch succeeded; want an error mentioning %v", tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error missing %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), "can't find pane") {
					t.Errorf("error is the old vanished-pane failure: %v", err)
				}
			}

			list, err := store.ListWorkspaces(ctx)
			if err != nil || len(list) != 1 {
				t.Fatalf("ListWorkspaces = %v, %v", list, err)
			}
			if list[0].Status != tc.wantWSStatus {
				t.Errorf("workspace status = %q, want %q", list[0].Status, tc.wantWSStatus)
			}
			tasks, err := store.ListTasksByWorkspace(ctx, list[0].ID)
			if err != nil || len(tasks) == 0 {
				t.Fatalf("ListTasksByWorkspace = %v, %v", tasks, err)
			}
			prov := tasks[0]
			if prov.Kind != registry.TaskKindCommand || prov.Status != tc.wantProvTasks {
				t.Errorf("provisioning task = %+v, want command task %q", prov, tc.wantProvTasks)
			}
			if prov.Status == registry.TaskStatusCompleted {
				if alive, _ := exec.HasSession(ctx, prov.TmuxSession); alive {
					t.Errorf("provisioning session %s left running after it completed", prov.TmuxSession)
				}
			}
		})
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// killWorkspaceSessions removes every tmux session this test's tasks
// created, including failed ones deliberately left for inspection.
func killWorkspaceSessions(t *testing.T, store registry.Store, exec targets.TargetExecutor) {
	t.Helper()
	tasks, err := store.ListTasks(context.Background())
	if err != nil {
		return
	}
	for _, task := range tasks {
		_ = exec.KillSession(context.Background(), task.TmuxSession)
	}
}
