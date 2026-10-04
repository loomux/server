package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-88: the routing model sees each workspace's status, target,
// recent summary and last use, most recently used first — and never a
// failed or archived one.
func TestDispatch_WorkspaceSnapshotEnriched(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	older := time.Now().Add(-48 * time.Hour).UTC()
	newer := time.Now().Add(-time.Hour).UTC()
	long := strings.Repeat("x", 300)
	for _, ws := range []*registry.Workspace{
		{ID: "ws-old", Name: "old", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle, LastUsedAt: &older, RollingSummary: long},
		{ID: "ws-new", Name: "new", TargetID: h.target.ID, Status: registry.WorkspaceStatusActive, LastUsedAt: &newer, RollingSummary: "added the rate limiter"},
		{ID: "ws-never", Name: "never", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle},
		{ID: "ws-failed", Name: "failed", TargetID: h.target.ID, Status: registry.WorkspaceStatusFailed},
		{ID: "ws-archived", Name: "archived", TargetID: h.target.ID, Status: registry.WorkspaceStatusArchived},
	} {
		if err := h.store.CreateWorkspace(ctx, ws); err != nil {
			t.Fatalf("CreateWorkspace %s: %v", ws.ID, err)
		}
	}
	var offered []router.WorkspaceSnapshot
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		offered = workspaces
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := h.r.Dispatch(ctx, "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	var ids []string
	for _, ws := range offered {
		ids = append(ids, ws.ID)
	}
	if strings.Join(ids, ",") != "ws-new,ws-old,ws-never" {
		t.Fatalf("offered workspaces = %v, want ws-new,ws-old,ws-never (recent first, no failed/archived)", ids)
	}
	got := offered[0]
	if got.Status != "active" || got.TargetName != "jet01" || got.Summary != "added the rate limiter" ||
		got.LastUsed == nil || !got.LastUsed.Equal(newer) {
		t.Errorf("ws-new snapshot = %+v", got)
	}
	if s := offered[1].Summary; len([]rune(s)) != router.SnapshotSummaryRunes+1 || !strings.HasSuffix(s, "…") {
		t.Errorf("long summary = %d runes %q, want truncated to %d plus …", len([]rune(s)), s, router.SnapshotSummaryRunes)
	}
}

// LOOM-88: the target snapshot carries each recorded agent's version.
func TestDispatch_TargetSnapshotCarriesAgentVersions(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	if err := h.store.SetTargetAgent(ctx, &registry.TargetAgent{
		TargetID: h.target.ID, AgentType: "claude-code", Available: true, Path: "/usr/bin/claude", Version: "2.1.4 (Claude Code)",
	}); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if v := h.model.LastDecideTargets[0].AgentVersions["claude-code"]; v != "2.1.4 (Claude Code)" {
		t.Errorf("claude-code version in snapshot = %q", v)
	}
}

func (h *availabilityHarness) recordAgents(t *testing.T, available map[string]bool) {
	t.Helper()
	for agent, ok := range available {
		if err := h.store.SetTargetAgent(context.Background(), &registry.TargetAgent{
			TargetID: h.target.ID, AgentType: agent, Available: ok,
		}); err != nil {
			t.Fatalf("SetTargetAgent: %v", err)
		}
	}
}

// LOOM-88: the router chose an agent the target is recorded not to have,
// and the user didn't ask for it: an installed one is used instead, the
// reply says so, and the routing decision log records the substitution.
func TestDispatch_UnavailableAgentNotNamed_Substituted(t *testing.T) {
	logs := &logBuffer{}
	h := newAvailabilityHarnessOpts(t, nil, router.WithLogger(logs.logger()))
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"codex": false, "claude-code": true})
	h.decide(h.provisionDecision("codex"))

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "set up a scratch project")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.HasPrefix(reply, "Using claude-code: codex isn't installed on jet01.") || !strings.Contains(reply, "relayed") {
		t.Errorf("reply = %q, want the substitution note then the agent's reply", reply)
	}
	ws := h.workspaces(t)
	if len(ws) != 1 {
		t.Fatalf("workspaces = %+v, want the provisioned one", ws)
	}
	tasks, _ := h.store.ListTasksByWorkspace(context.Background(), ws[0].ID)
	for _, task := range tasks {
		if task.AgentType == "codex" {
			t.Errorf("a codex task was written: %+v", task)
		}
	}
	if !hasAgentTask(tasks, "claude-code") {
		t.Errorf("tasks = %+v, want a claude-code task", tasks)
	}
	msgs, _ := h.store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(msgs) != 2 || !strings.HasPrefix(msgs[1].Content, "Using claude-code:") {
		t.Errorf("stored reply = %+v, want the note kept in history too", msgs)
	}
	var found bool
	for _, rec := range logs.records(t) {
		if rec["msg"] == "routing decision" && rec["agent_substituted_from"] == "codex" && rec["agent_type"] == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Errorf("no routing decision log with agent_substituted_from=codex:\n%s", logs.buf.String())
	}
}

func hasAgentTask(tasks []*registry.Task, agentType string) bool {
	for _, task := range tasks {
		if task.Kind == registry.TaskKindAgent && task.AgentType == agentType {
			return true
		}
	}
	return false
}

// The same choice on an existing workspace: the turn goes to the
// installed agent, and no task is written for the missing one.
func TestDispatch_UnavailableAgentNotNamed_UseWorkspaceSubstituted(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"codex": false, "claude-code": true})
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "fix the bug"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tasks, _ := h.store.ListTasksByWorkspace(context.Background(), ws.ID)
	if len(tasks) != 1 || tasks[0].AgentType != "claude-code" {
		t.Fatalf("tasks = %+v, want one claude-code task", tasks)
	}
	// The substitute is told why it got the work (sent typed or on its
	// command line, whichever the profile uses), while history keeps the
	// user's own words.
	s := h.exec.sessionFor(tasks[0].TmuxSession)
	sent := s.command + strings.Join(s.keys, "\n")
	note := "[Loomux note: this request was routed to codex, which isn't installed on jet01, so you (claude-code) are handling it instead.]"
	if !strings.Contains(sent, note) || !strings.Contains(sent, "fix the bug") {
		t.Errorf("the agent got %q, want the note and the message", sent)
	}
	msgs, _ := h.store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(msgs) == 0 || msgs[0].Content != "fix the bug" {
		t.Errorf("stored messages = %+v, want the user's message as typed", msgs)
	}
}

// The user asked for the missing agent by name: that's a request for
// it, so it's the LOOM-71 install offer, with nothing written.
func TestDispatch_UnavailableAgentNamed_InstallOffer(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.recordAgents(t, map[string]bool{"codex": false, "claude-code": true})
	h.decide(h.provisionDecision("codex"))

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "use Codex to set up a scratch project")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, codexInstall) {
		t.Errorf("reply = %q, want the install offer", reply)
	}
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspace rows written: %+v", ws)
	}
}

// Nothing else is installed there: the install offer, nothing written.
func TestDispatch_UnavailableAgentNoAlternative_InstallOffer(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.recordAgents(t, map[string]bool{"codex": false, "claude-code": false})
	h.decide(h.provisionDecision("codex"))

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "set up a scratch project")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, codexInstall) {
		t.Errorf("reply = %q, want the install offer", reply)
	}
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspace rows written: %+v", ws)
	}
}

func TestAgentNamedIn(t *testing.T) {
	entry := router.AgentType{Binary: "claude"}
	for msg, want := range map[string]bool{
		"ask claude to fix it":         true,
		"use Claude-Code here":         true,
		"run it with claude-code":      true,
		"claudette is my cat":          false,
		"no agent mentioned":           false,
		"(claude) please":              true,
		"use codex, not the other one": false,
	} {
		if got := router.AgentNamedIn(msg, "claude-code", entry); got != want {
			t.Errorf("AgentNamedIn(%q) = %v, want %v", msg, got, want)
		}
	}
}
