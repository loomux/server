package llmrouter

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

func TestDecide_AnswerDirectly(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "Hello there!",
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "Hello there!"}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decide = %+v, want %+v", dec, want)
	}
}

func TestDecide_UseWorkspace(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":       "use_workspace",
		"workspace_id": "ws-1",
		"agent_type":   "claude-code",
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workspaces := []router.WorkspaceSnapshot{{ID: "ws-1", Name: "Test", Description: "d"}}
	dec, err := m.Decide(context.Background(), "do the thing", workspaces)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: "ws-1", AgentType: "claude-code"}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decide = %+v, want %+v", dec, want)
	}
}

// TestDecide_WorkspaceHint_AppearsInPromptAdvisoryOnly proves
// router.WithWorkspaceHint (LOOM-46) reaches the actual LLM call as
// advisory text in the user prompt, but never overrides the model's own
// use_workspace decision — the model here picks a workspace other than
// the hinted one, and Decide returns that unmodified.
func TestDecide_WorkspaceHint_AppearsInPromptAdvisoryOnly(t *testing.T) {
	var gotBody []byte
	handler := toolCallHandler(t, decideToolName, map[string]any{
		"action":       "use_workspace",
		"workspace_id": "ws-other",
		"agent_type":   "claude-code",
	})
	srv, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		handler(w, r)
	})

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workspaces := []router.WorkspaceSnapshot{{ID: "ws-hinted", Name: "Hinted"}, {ID: "ws-other", Name: "Other"}}
	dec, err := m.Decide(context.Background(), "do the thing", workspaces, router.WithWorkspaceHint("ws-hinted"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: "ws-other", AgentType: "claude-code"}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decide = %+v, want %+v (the hint must be advisory, not binding)", dec, want)
	}
	if !strings.Contains(string(gotBody), "ws-hinted") {
		t.Errorf("request body did not mention the hinted workspace_id %q: %s", "ws-hinted", gotBody)
	}
}

func TestDecide_ProvisionWorkspace(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":     "provision_workspace",
		"agent_type": "claude-code",
		"new_workspace": map[string]any{
			"name":              "new-ws",
			"path":              "/path",
			"target_id":         "target-1",
			"git_remote":        "git@example.com:foo/bar.git",
			"description":       "a new workspace",
			"tags":              []string{"a", "b"},
			"provision_command": "git clone ...",
		},
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "start a new project", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{
		Action:    router.ActionProvisionWorkspace,
		AgentType: "claude-code",
		NewWorkspace: router.ProvisionSpec{
			Name: "new-ws", Path: "/path", TargetID: "target-1", GitRemote: "git@example.com:foo/bar.git",
			Description: "a new workspace", Tags: []string{"a", "b"}, ProvisionCommand: "git clone ...",
		},
	}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decide = %+v, want %+v", dec, want)
	}
}

func TestDecide_MalformedPrimary_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, primaryCount := newFakeServer(t, malformedToolCallHandler(t, decideToolName))
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from escalation",
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
	if *primaryCount == 0 {
		t.Error("primary server was never called")
	}
	if *escalationCount == 0 {
		t.Error("escalation server was never called")
	}
}

func TestDecide_PrimaryTransportError_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from escalation",
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
}

func TestDecide_BothTiersFail_ReturnsCombinedError(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))
	escalationSrv, _ := newFakeServer(t, errorHandler(500))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Decide(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("Decide: want error, got nil")
	}
	if !strings.Contains(err.Error(), "primary model failed") || !strings.Contains(err.Error(), "escalation model also failed") {
		t.Errorf("Decide err = %q, want it to mention both primary and escalation failures", err.Error())
	}
}

func TestDecide_PrimaryFails_NoEscalationConfigured_SurfacesDirectly(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))

	m, err := New(Config{Primary: tierFor(primarySrv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Decide(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("Decide: want error, got nil")
	}
	if strings.Contains(err.Error(), "escalation") {
		t.Errorf("Decide err = %q, want no mention of escalation since none is configured", err.Error())
	}
}

func TestDecide_PrimarySucceeds_EscalationNeverCalled(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from primary",
	}))
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from escalation",
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from primary" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from primary")
	}
	if *escalationCount != 0 {
		t.Errorf("escalation server was called %d times, want 0", *escalationCount)
	}
}

func TestDecide_UnknownAgentType_EscalatesAsValidationFailure(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":       "use_workspace",
		"workspace_id": "ws-1",
		"agent_type":   "not-a-real-agent-type",
	}))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from escalation",
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workspaces := []router.WorkspaceSnapshot{{ID: "ws-1"}}
	dec, err := m.Decide(context.Background(), "hi", workspaces)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
}

func TestDecide_UnknownWorkspaceID_EscalatesAsValidationFailure(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":       "use_workspace",
		"workspace_id": "not-a-real-workspace",
		"agent_type":   "claude-code",
	}))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "from escalation",
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workspaces := []router.WorkspaceSnapshot{{ID: "ws-1"}}
	dec, err := m.Decide(context.Background(), "hi", workspaces)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
}

func TestDecide_NoToolCall_TreatedAsFailure(t *testing.T) {
	primarySrv, _ := newFakeServer(t, noToolCallHandler(t))

	m, err := New(Config{Primary: tierFor(primarySrv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Decide(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("Decide: want error, got nil")
	}
}

func TestNew_EmptyAgentTypes_StillConstructs(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "ok",
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New with empty agentTypes: %v", err)
	}
	if m == nil {
		t.Fatal("New returned nil Model with nil error")
	}
}
