package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func (h *availabilityHarness) setPolicy(t *testing.T, p registry.TargetPolicy) {
	t.Helper()
	got, err := h.store.GetTarget(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	got.Policy = p
	if err := h.store.UpdateTarget(context.Background(), got); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
}

func (h *availabilityHarness) policyWorkspace(t *testing.T) *registry.Workspace {
	t.Helper()
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}

func (h *availabilityHarness) noSideEffects(t *testing.T) {
	t.Helper()
	if ws := h.workspaces(t); len(ws) != 0 && ws[0].Name != "existing" {
		t.Errorf("workspaces = %+v, want nothing written", ws)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Errorf("launched %v, want nothing", cmds)
	}
}

// LOOM-89: a decision the target's policy forbids is refused with a clear
// reply, before anything runs.
func TestPolicy_RefusesForbiddenDecisions(t *testing.T) {
	cases := []struct {
		name   string
		policy registry.TargetPolicy
		decide func(h *availabilityHarness, ws *registry.Workspace) router.Decision
		want   string
	}{
		{"provision forbidden", registry.TargetPolicy{NoProvision: true},
			func(h *availabilityHarness, _ *registry.Workspace) router.Decision {
				return h.provisionDecision("codex")
			},
			"new workspaces"},
		{"shell forbidden", registry.TargetPolicy{NoShell: true},
			func(h *availabilityHarness, _ *registry.Workspace) router.Decision {
				return router.Decision{Action: router.ActionRunCommand, TargetID: h.target.ID, Command: "uptime"}
			}, "shell commands"},
		{"agent not allowed", registry.TargetPolicy{AllowedAgentTypes: []string{"claude-code"}},
			func(h *availabilityHarness, ws *registry.Workspace) router.Decision {
				return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"}
			}, "claude-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAvailabilityHarness(t)
			h.probes.install("codex")
			h.probes.install("claude")
			ws := h.policyWorkspace(t)
			h.setPolicy(t, tc.policy)
			h.decide(tc.decide(h, ws))

			reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on jet01")
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if !strings.Contains(reply, "jet01") || !strings.Contains(reply, tc.want) {
				t.Errorf("reply = %q, want a refusal naming jet01 and %q", reply, tc.want)
			}
			h.noSideEffects(t)
			if ws := h.workspaces(t); len(ws) != 1 {
				t.Errorf("workspaces = %+v, want only the existing one", ws)
			}
		})
	}
}

// A require-confirmation target shows the plan and waits for a "yes";
// nothing runs before it, and the yes carries out exactly that plan.
func TestPolicy_RequireConfirmation_ProvisionRunsOnYes(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.setPolicy(t, registry.TargetPolicy{Purpose: registry.TargetPurposeWork, RequireConfirmation: true})
	h.decide(h.provisionDecision("codex"))
	ctx := context.Background()

	reply, err := h.r.Dispatch(ctx, "conv-1", "set up a workspace")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "jet01") || !strings.Contains(reply, "hostname-uptime") || !strings.Contains(reply, `"yes"`) {
		t.Errorf("reply = %q, want the plan and a request for yes", reply)
	}
	h.noSideEffects(t)
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Fatalf("workspaces = %+v before confirmation", ws)
	}

	decidesBefore := h.decides
	if _, err := h.r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	if h.decides != decidesBefore {
		t.Errorf("the confirmation was routed again (%d decides), want the offered plan carried out", h.decides-decidesBefore)
	}
	if ws := h.workspaces(t); len(ws) != 1 || ws[0].Name != "hostname-uptime" {
		t.Errorf("workspaces = %+v, want the planned one", ws)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) == 0 {
		t.Error("nothing launched after the confirmation")
	}
}

func TestPolicy_RequireConfirmation_DeclineRunsNothing(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	h.decide(h.provisionDecision("codex"))
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "set up a workspace"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok, cancelled"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "no, never mind"); err != nil {
		t.Fatalf("Dispatch(no): %v", err)
	}
	h.noSideEffects(t)
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspaces = %+v after a decline", ws)
	}
}

// The policy is checked again at the "yes": tightened since the offer,
// the plan is refused.
func TestPolicy_RequireConfirmation_RecheckedOnYes(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	h.decide(h.provisionDecision("codex"))
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "set up a workspace"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true, NoProvision: true})
	reply, err := h.r.Dispatch(ctx, "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	if !strings.Contains(reply, "new workspaces") {
		t.Errorf("reply = %q, want the refusal", reply)
	}
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspaces = %+v, want none", ws)
	}
}

// Follow-up turns in the conversation's open pane aren't asked about
// again: the work there was already confirmed.
func TestPolicy_RequireConfirmation_NotAskedForFollowUps(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.policyWorkspace(t)
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "start"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := h.r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	launched := len(h.exec.launchedCommands())
	if launched != 1 {
		t.Fatalf("launched %d after the confirmation, want 1", launched)
	}
	reply, err := h.r.Dispatch(ctx, "conv-1", "and now the tests")
	if err != nil {
		t.Fatalf("Dispatch(follow-up): %v", err)
	}
	if strings.Contains(reply, `"yes"`) {
		t.Errorf("follow-up asked for confirmation again: %q", reply)
	}
}

// Every other offer is re-checked against the policy at its "yes" too: an
// install, a clone the user didn't name and a command not given verbatim
// were all allowed when offered, then the policy was tightened.
func TestPolicy_OffersRecheckedOnYes(t *testing.T) {
	ctx := context.Background()
	confirmRefused := func(t *testing.T, h *availabilityHarness, want string) {
		t.Helper()
		reply, err := h.r.Dispatch(ctx, "conv-1", "yes")
		if err != nil {
			t.Fatalf("Dispatch(yes): %v", err)
		}
		if !strings.Contains(reply, want) {
			t.Errorf("reply = %q, want the refusal (%q)", reply, want)
		}
		h.noSideEffects(t)
	}
	offered := func(t *testing.T, h *availabilityHarness, message string) {
		t.Helper()
		reply, err := h.r.Dispatch(ctx, "conv-1", message)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if !strings.Contains(reply, `"yes"`) {
			t.Fatalf("no offer made:\n%s", reply)
		}
	}

	t.Run("install, provisioning since forbidden", func(t *testing.T) {
		h := newAvailabilityHarness(t)
		h.decide(h.provisionDecision("codex"))
		offered(t, h, "run hostname on jet01 with codex")
		h.setPolicy(t, registry.TargetPolicy{NoProvision: true})
		confirmRefused(t, h, "new workspaces")
	})
	t.Run("install, agent since disallowed", func(t *testing.T) {
		h := newAvailabilityHarness(t)
		ws := h.policyWorkspace(t)
		h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
		offered(t, h, "use codex there")
		h.setPolicy(t, registry.TargetPolicy{AllowedAgentTypes: []string{"claude-code"}})
		confirmRefused(t, h, "allows only claude-code")
	})
	t.Run("clone, provisioning since forbidden", func(t *testing.T) {
		h := newAvailabilityHarness(t)
		h.probes.install("codex")
		h.decide(router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "codex", NewWorkspace: router.ProvisionSpec{
			Name: "server", TargetID: h.target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://github.com/loomux/server.git",
		}})
		offered(t, h, "set up the loomux server repo")
		h.setPolicy(t, registry.TargetPolicy{NoProvision: true})
		confirmRefused(t, h, "new workspaces")
	})
	t.Run("command, shell since forbidden", func(t *testing.T) {
		h := newCommandHarness(t)
		h.decideCommand("hostname && uptime")
		h.outputs["hostname && uptime"] = "jet01"
		offered(t, h.availabilityHarness, "how long has jet01 been up?")
		h.setPolicy(t, registry.TargetPolicy{NoShell: true})
		confirmRefused(t, h.availabilityHarness, "shell commands")
		if n := len(h.commandTasks(t)); n != 0 {
			t.Errorf("%d command tasks, want none", n)
		}
	})
}

// LOOM-122: a follow-up whose pane is gone (reaped, killed, or its agent
// exited) would launch a fresh agent, so it is new work and asked about
// again; only a live pane carries the earlier confirmation.
func TestPolicy_RequireConfirmation_AskedAgainWhenThePaneIsGone(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.policyWorkspace(t)
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "start"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := h.r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	tasks, err := h.store.ListTasksByWorkspace(ctx, ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v; want the one launched", tasks, err)
	}
	if err := h.exec.KillSession(ctx, tasks[0].TmuxSession); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	reply, err := h.r.Dispatch(ctx, "conv-1", "and now the tests")
	if err != nil {
		t.Fatalf("Dispatch(follow-up): %v", err)
	}
	if !strings.Contains(reply, `"yes"`) {
		t.Errorf("relaunch into a dead pane's workspace wasn't confirmed: %q", reply)
	}
	if n := len(h.exec.launchedCommands()); n != 1 {
		t.Errorf("launched %d, want still only the first", n)
	}
}
