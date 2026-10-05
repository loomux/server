package router_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestSecretsNeverLeakIntoCapturedOutputOrSummary is the test LOOM-7's
// handoff flagged as still owed: the design spec's Testing Strategy
// section requires "an explicit test that secrets never appear in
// captured pane output or the router's relayed chat text — a
// security-relevant test, not just a functional one." That needed a
// real Launch/CapturePane/Relay path to test against, which this ticket
// is the first to provide.
//
// The launched "agent" is a well-behaved credential consumer: it reads
// its secret from the environment but never prints the raw value, only
// its length (echo "len=${#SECRET_NAME}") — mirroring how a real coding
// agent would use a token to authenticate against an API without
// echoing it to its own terminal transcript. This gives the test both:
//   - a negative assertion (the raw secret value never appears in the
//     reply or the rolling summary) — the actual security property, and
//   - a positive control (the reply DOES show the expected len=<N>
//     marker) — proof the secret really was injected via the real
//     credentials.Resolver + credentials.ShellEnvPrefix pipeline, so the
//     negative assertion isn't trivially true because injection
//     silently didn't happen at all.
func TestSecretsNeverLeakIntoCapturedOutputOrSummary(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := &registry.Target{ID: uuid.NewString(), Name: "secrets-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID:       uuid.NewString(),
		Name:     "secrets-ws-" + t.Name(),
		Path:     t.TempDir(),
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	secretValue := "sk-distinctive-secret-value-abc123xyz"
	cred := &registry.Credential{
		ID:          uuid.NewString(),
		Name:        "MY_SECRET",
		WorkspaceID: ws.ID,
		Value:       secretValue,
	}
	if err := store.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	agentTypes := router.AgentTypeRegistry{
		"well-behaved-agent": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 300 * time.Millisecond},
			LaunchTemplate: `sh -c 'echo "len=${#MY_SECRET}"; sleep 30'`,
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "well-behaved-agent"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			// A trivial passthrough relay — the point of this test is
			// what the PANE showed, not clever condensation.
			return router.RelayResult{Reply: strings.TrimSpace(captured), Done: true}, nil
		},
	}

	r := router.New(store, orch, targets.NewExecutor, resolver, agentTypes, model, markerDir)

	reply, err := r.Dispatch(ctx, "conv-secrets", "authenticate and do the thing")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Negative assertion: the raw secret must never appear anywhere
	// downstream of the launch.
	if strings.Contains(reply, secretValue) {
		t.Fatalf("secret value leaked into the relayed chat reply: %q", reply)
	}
	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if strings.Contains(updatedWS.RollingSummary, secretValue) {
		t.Fatalf("secret value leaked into the workspace rolling summary: %q", updatedWS.RollingSummary)
	}

	// LOOM-113: the secret never sits on a command line. tmux keeps the
	// pane's start command (and ps shows it while it runs), so it must
	// not be there, and the file it came from is gone once read.
	tasks, err := store.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasks = %v, %v", tasks, err)
	}
	startCmd, err := exec.Command("tmux", "-L", targets.TmuxSocket, "display-message", "-p", "-t",
		tasks[0].TmuxSession, "#{pane_start_command}").CombinedOutput()
	if err != nil {
		t.Fatalf("tmux display-message: %v: %s", err, startCmd)
	}
	if strings.Contains(string(startCmd), secretValue) {
		t.Fatalf("secret value is on the pane's command line: %s", startCmd)
	}
	if left, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".loomux", "env", "*")); len(left) != 0 {
		t.Errorf("env files left behind: %v", left)
	}

	// Positive control: the agent really did receive the secret via
	// injected env, proving the negative assertions above aren't
	// trivially true because injection silently didn't happen.
	wantMarker := fmt.Sprintf("len=%d", len(secretValue))
	if !strings.Contains(reply, wantMarker) {
		t.Fatalf("reply = %q, want it to contain %q (proof the secret was actually injected)", reply, wantMarker)
	}
}
