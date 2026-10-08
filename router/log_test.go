package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// secretishMessage stands in for a chat message body. LOOM-63: message
// bodies are user content (and may contain anything a user pastes), so
// they must never reach the logs at info level.
const secretishMessage = "please deploy with token hunter2-do-not-log"

// logBuffer collects a Router's JSON log output at info level — the
// level production runs at by default.
type logBuffer struct{ buf bytes.Buffer }

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (l *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// find returns the first record with the given msg, failing the test if
// there is none.
func (l *logBuffer) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	recs := l.records(t)
	for _, rec := range recs {
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q; got %v", msg, recs)
	return nil
}

func (l *logBuffer) assertNoMessageBody(t *testing.T) {
	t.Helper()
	if strings.Contains(l.buf.String(), "hunter2") {
		t.Fatalf("message body leaked into logs: %s", l.buf.String())
	}
}

func assertAttr(t *testing.T, rec map[string]any, key string, want any) {
	t.Helper()
	if got := rec[key]; got != want {
		t.Errorf("record %q: %s = %v, want %v (record: %v)", rec["msg"], key, got, want, rec)
	}
}

func setupLogged(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *routertest.StubRoutingModel, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	store := newTestStore(t)
	exec, r, model := newRouter(t, store, router.WithLogger(logs.logger()))
	return store, exec, r, model, logs
}

func TestDispatch_Logs_RoutingDecision_AnswerDirectly(t *testing.T) {
	_, _, r, model, logs := setupLogged(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	start := logs.find(t, "dispatch started")
	assertAttr(t, start, "conversation_id", "conv-1")
	assertAttr(t, start, "message_len", float64(len(secretishMessage)))

	dec := logs.find(t, "routing decision")
	assertAttr(t, dec, "level", "INFO")
	assertAttr(t, dec, "conversation_id", "conv-1")
	assertAttr(t, dec, "action", "answer_directly")

	logs.assertNoMessageBody(t)
}

func TestDispatch_Logs_ProvisionAndAgentDispatch(t *testing.T) {
	store, _, r, model, logs := setupLogged(t)
	target := createFixtureTarget(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action: router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{
				Name:     "new-ws",
				TargetID: target.ID, Kind: router.ProvisionEmpty,
			},
			AgentType: "claude-code",
		}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "done", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	list, err := store.ListWorkspaces(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %v, %v; want exactly one", list, err)
	}
	wsID := list[0].ID

	dec := logs.find(t, "routing decision")
	assertAttr(t, dec, "action", "provision_workspace")
	assertAttr(t, dec, "agent_type", "claude-code")
	assertAttr(t, dec, "target_id", target.ID)
	assertAttr(t, dec, "workspace_name", "new-ws")

	prov := logs.find(t, "provisioning workspace")
	assertAttr(t, prov, "workspace_id", wsID)
	assertAttr(t, prov, "target_id", target.ID)

	done := logs.find(t, "workspace provisioned")
	assertAttr(t, done, "workspace_id", wsID)

	started := logs.find(t, "agent dispatch started")
	assertAttr(t, started, "workspace_id", wsID)
	assertAttr(t, started, "agent_type", "claude-code")
	assertAttr(t, started, "resumed", false)
	if started["task_id"] == nil || started["task_id"] == "" {
		t.Errorf("agent dispatch started: missing task_id: %v", started)
	}

	finished := logs.find(t, "agent dispatch finished")
	assertAttr(t, finished, "workspace_id", wsID)
	assertAttr(t, finished, "task_id", started["task_id"])
	assertAttr(t, finished, "task_done", true)
	if _, ok := finished["duration_ms"]; !ok {
		t.Errorf("agent dispatch finished: missing duration_ms: %v", finished)
	}

	logs.assertNoMessageBody(t)
}

// TestDispatch_Logs_ProvisioningFailure_AtError covers the incident shape
// LOOM-63 was filed for: a workspace left stuck in provisioning with
// nothing in the logs.
func TestDispatch_Logs_ProvisioningFailure_AtError(t *testing.T) {
	store, exec, r, model, logs := setupLogged(t)
	target := createFixtureTarget(t, store)
	exec.unreachable = true
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action:       router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{Name: "doomed-ws", TargetID: target.ID, Kind: router.ProvisionEmpty},
			AgentType:    "claude-code",
		}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err == nil {
		t.Fatalf("Dispatch: got nil error")
	}
	list, err := store.ListWorkspaces(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %v, %v; want the stuck row", list, err)
	}

	rec := logs.find(t, "provisioning failed")
	assertAttr(t, rec, "level", "ERROR")
	assertAttr(t, rec, "workspace_id", list[0].ID)
	// The row's persisted status, not the router's local copy: a failed
	// launch has orchestrator.failTask revert it to idle.
	assertAttr(t, rec, "workspace_status", string(list[0].Status))
	if rec["error"] == nil || rec["error"] == "" {
		t.Errorf("provisioning failed: missing error: %v", rec)
	}
	logs.assertNoMessageBody(t)
}

func TestDispatch_Logs_UnresolvableTarget_AtError(t *testing.T) {
	store, _, r, model, logs := setupLogged(t)
	// Some other target is registered: with none at all, Dispatch answers
	// "register a target first" before provisioning is reached (LOOM-68).
	createFixtureTarget(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action:       router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{Name: "x", TargetID: "sc1", Kind: router.ProvisionEmpty},
			AgentType:    "claude-code",
		}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err == nil {
		t.Fatalf("Dispatch: got nil error")
	}

	rec := logs.find(t, "provisioning failed")
	assertAttr(t, rec, "level", "ERROR")
	assertAttr(t, rec, "target_id", "sc1")
	if _, ok := rec["workspace_id"]; ok {
		t.Errorf("no workspace row should exist, but the record names one: %v", rec)
	}
}

func TestDispatch_Logs_RoutingFailure_AtError(t *testing.T) {
	_, _, r, model, logs := setupLogged(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{}, errors.New("model unavailable")
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err == nil {
		t.Fatalf("Dispatch: got nil error")
	}

	rec := logs.find(t, "routing failed")
	assertAttr(t, rec, "level", "ERROR")
	assertAttr(t, rec, "conversation_id", "conv-1")
	if !strings.Contains(rec["error"].(string), "model unavailable") {
		t.Errorf("routing failed: error = %v, want it to carry the cause", rec["error"])
	}
	logs.assertNoMessageBody(t)
}

func TestDispatch_Logs_AgentDispatchFailure_AtError(t *testing.T) {
	store, _, r, model, logs := setupLogged(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{}, errors.New("relay model down")
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err == nil {
		t.Fatalf("Dispatch: got nil error")
	}

	rec := logs.find(t, "agent dispatch failed")
	assertAttr(t, rec, "level", "ERROR")
	assertAttr(t, rec, "workspace_id", ws.ID)
	if !strings.Contains(rec["error"].(string), "relay model down") {
		t.Errorf("agent dispatch failed: error = %v, want it to carry the cause", rec["error"])
	}
	logs.assertNoMessageBody(t)
}

// TestDispatch_Logs_ProvisioningFailure_AfterLaunch_ReportsPersistedStatus
// covers failures after the provisioning session started: the failure
// record must report the row's actual persisted status (failed), not the
// router's stale "provisioning" copy (LOOM-63 review). Provisioning is a
// command task (LOOM-90), so it fails by exiting non-zero, or by its
// teardown failing.
func TestDispatch_Logs_ProvisioningFailure_AfterLaunch_ReportsPersistedStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stage     string
		breakExec func(*fakeExecutor)
	}{
		{name: "non-zero exit", stage: "run", breakExec: func(e *fakeExecutor) {
			e.paneExit = func(command string) *targets.PaneExit {
				if isProvisioning(command) {
					return &targets.PaneExit{Status: 1, Output: "boom"}
				}
				return nil
			}
		}},
		{name: "teardown", stage: "run", breakExec: func(e *fakeExecutor) { e.killErr = errors.New("kill broke") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, exec, r, model, logs := setupLogged(t)
			target := createFixtureTarget(t, store)
			tc.breakExec(exec)
			model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
				return router.Decision{
					Action:       router.ActionProvisionWorkspace,
					NewWorkspace: router.ProvisionSpec{Name: "stuck-ws", TargetID: target.ID, Kind: router.ProvisionEmpty},
					AgentType:    "claude-code",
				}, nil
			}

			if _, err := r.Dispatch(context.Background(), "conv-1", secretishMessage); err == nil {
				t.Fatalf("Dispatch: got nil error")
			}
			list, err := store.ListWorkspaces(context.Background())
			if err != nil || len(list) != 1 {
				t.Fatalf("ListWorkspaces = %v, %v; want the stuck row", list, err)
			}
			persisted := string(list[0].Status)
			if persisted == string(registry.WorkspaceStatusProvisioning) {
				t.Fatalf("precondition: row status is still provisioning; this test needs a post-launch failure")
			}

			rec := logs.find(t, "provisioning failed")
			assertAttr(t, rec, "level", "ERROR")
			assertAttr(t, rec, "stage", tc.stage)
			assertAttr(t, rec, "workspace_id", list[0].ID)
			assertAttr(t, rec, "workspace_status", persisted)
			logs.assertNoMessageBody(t)
		})
	}
}

// TestWithLogger_Nil_KeepsDefault: WithLogger(nil) must not leave the
// Router with a nil logger that panics on first use.
func TestWithLogger_Nil_KeepsDefault(t *testing.T) {
	store := newTestStore(t)
	_, r, model := newRouter(t, store, router.WithLogger(nil))
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
}

// TestDispatch_Logs_NeverIncludeTargetHost guards the decision to keep
// internal hostnames out of what leaves the server's control (LOOM-64):
// a full provision + agent dispatch on a remote target logs it by
// target_id only — never its host or user.
func TestDispatch_Logs_NeverIncludeTargetHost(t *testing.T) {
	store, _, r, model, logs := setupLogged(t)
	remote := &registry.Target{
		ID:        uuid.NewString(),
		Name:      "bigbox",
		Kind:      registry.TargetKindRemote,
		Host:      "bigbox.tailnet.example.invalid",
		User:      "loomux-remote-user",
		SSHKeyRef: createBigboxKey(t, store),
	}
	if err := store.CreateTarget(context.Background(), remote); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{
			Action:       router.ActionProvisionWorkspace,
			NewWorkspace: router.ProvisionSpec{Name: "remote-ws", TargetID: remote.ID, Kind: router.ProvisionEmpty},
			AgentType:    "claude-code",
		}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "done", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	assertAttr(t, logs.find(t, "provisioning workspace"), "target_id", remote.ID)
	for _, unwanted := range []string{remote.Host, remote.User, remote.SSHKeyRef} {
		if strings.Contains(logs.buf.String(), unwanted) {
			t.Errorf("logs contain %q: %s", unwanted, logs.buf.String())
		}
	}
}
