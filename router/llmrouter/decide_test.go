package llmrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
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

	dec, err := m.Decide(context.Background(), "hi", nil, nil)
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
	dec, err := m.Decide(context.Background(), "do the thing", workspaces, nil)
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
	dec, err := m.Decide(context.Background(), "do the thing", workspaces, nil, router.WithWorkspaceHint("ws-hinted"))
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
			"name":        "new-ws",
			"target_id":   "target-1",
			"kind":        "git_clone",
			"git_remote":  "git@example.com:foo/bar.git",
			"description": "a new workspace",
			"tags":        []string{"a", "b"},
		},
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dec, err := m.Decide(context.Background(), "start a new project", nil, []router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "local"}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{
		Action:    router.ActionProvisionWorkspace,
		AgentType: "claude-code",
		NewWorkspace: router.ProvisionSpec{
			Name: "new-ws", TargetID: "target-1", Kind: router.ProvisionGitClone, GitRemote: "git@example.com:foo/bar.git",
			Description: "a new workspace", Tags: []string{"a", "b"},
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

	dec, err := m.Decide(context.Background(), "hi", nil, nil)
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

	dec, err := m.Decide(context.Background(), "hi", nil, nil)
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

	_, err = m.Decide(context.Background(), "hi", nil, nil)
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

	_, err = m.Decide(context.Background(), "hi", nil, nil)
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

	dec, err := m.Decide(context.Background(), "hi", nil, nil)
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
	dec, err := m.Decide(context.Background(), "hi", workspaces, nil)
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
	dec, err := m.Decide(context.Background(), "hi", workspaces, nil)
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

	_, err = m.Decide(context.Background(), "hi", nil, nil)
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

// TestDecide_TargetsAppearInPrompt proves the registered targets reach
// the actual LLM call (LOOM-64): before this, the routing model was
// never told targets existed at all, so any target_id in a
// provision_workspace decision was a guess.
//
// And only what the model needs to choose one: id, name and kind.
func TestDecide_TargetsAppearInPrompt(t *testing.T) {
	var gotBody []byte
	handler := toolCallHandler(t, decideToolName, map[string]any{
		"action":        "answer_directly",
		"direct_answer": "ok",
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

	targets := []router.TargetSnapshot{
		{ID: "target-local", Name: "jet01", Kind: "local"},
		{ID: "target-remote", Name: "bigbox", Kind: "remote"},
	}
	if _, err := m.Decide(context.Background(), "hi", nil, targets); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	body := string(gotBody)
	for _, want := range []string{"target-local", "jet01", "local", "target-remote", "bigbox", "remote"} {
		if !strings.Contains(body, want) {
			t.Errorf("request body does not mention %q: %s", want, body)
		}
	}
	// The prompt goes to a third-party router vendor: targets are named
	// by id, name and kind only. Hosts (internal tailnet hostnames),
	// users and SSH key refs never leave the server.
	for _, unwanted := range []string{"bigbox.example.invalid", "host:", "user:", "ssh_key_ref"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("request body mentions %q, want it kept out of the router prompt: %s", unwanted, body)
		}
	}
}

func TestDecide_UnknownTargetID_EscalatesAsValidationFailure(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":     "provision_workspace",
		"agent_type": "claude-code",
		"new_workspace": map[string]any{
			"name":      "new-ws",
			"kind":      "empty",
			"target_id": "sc1",
		},
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

	targets := []router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "local"}}
	dec, err := m.Decide(context.Background(), "start a new project", nil, targets)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
}

func TestDecide_EmptyTargetID_EscalatesAsValidationFailure(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":     "provision_workspace",
		"agent_type": "claude-code",
		"new_workspace": map[string]any{
			"name": "new-ws",
			"kind": "empty",
		},
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

	targets := []router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "local"}}
	dec, err := m.Decide(context.Background(), "start a new project", nil, targets)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.DirectAnswer != "from escalation" {
		t.Errorf("Decide.DirectAnswer = %q, want %q", dec.DirectAnswer, "from escalation")
	}
}

// TestDecideUserPrompt_TargetAgentAvailability proves each target's
// recorded agent availability (LOOM-71) reaches the prompt — available,
// not installed, or (when never probed) not mentioned as either — and the
// system prompt tells the model what to do with it.
func TestDecideUserPrompt_TargetAgentAvailability(t *testing.T) {
	prompt := decideUserPrompt("hi", nil, []router.TargetSnapshot{
		{ID: "t1", Name: "jet01", Kind: "remote", Agents: map[string]bool{"codex": false, "claude-code": true}},
		{ID: "t2", Name: "bigbox", Kind: "remote"},
	}, router.DispatchOptions{})

	for _, want := range []string{
		"agents: claude-code: available, codex: not installed",
		"agents: not checked yet",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Index(prompt, "claude-code: available") > strings.Index(prompt, "bigbox") {
		t.Errorf("jet01's agents rendered under the wrong target:\n%s", prompt)
	}
	for _, want := range []string{"not installed", "offer to install"} {
		if !strings.Contains(decideSystemPrompt, want) {
			t.Errorf("system prompt doesn't explain agent availability (missing %q)", want)
		}
	}
}

// TestDecide_ProvisionSpecValidated (LOOM-90): a provision_workspace
// whose spec isn't something Loomux can safely provision — a path-like
// name, an unknown kind, a dangerous git remote — is a validation
// failure, escalated, never acted on; and the schema offers no free-form
// command or path at all.
func TestDecide_ProvisionSpecValidated(t *testing.T) {
	for _, ws := range []map[string]any{
		{"name": "../../.ssh", "target_id": "target-1", "kind": "empty"},
		{"name": "x", "target_id": "target-1", "kind": "shell"},
		{"name": "x", "target_id": "target-1", "kind": "git_clone", "git_remote": "ext::sh -c touch% /tmp/pwn"},
	} {
		primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
			"action": "provision_workspace", "agent_type": "claude-code", "new_workspace": ws,
		}))
		escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
			"action": "answer_directly", "direct_answer": "from escalation",
		}))
		escalation := tierFor(escalationSrv, "escalation-model")
		m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dec, err := m.Decide(context.Background(), "go", nil, []router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "remote"}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if dec.DirectAnswer != "from escalation" {
			t.Errorf("new_workspace %v: Decide = %+v, want rejected and escalated", ws, dec)
		}
	}

	tool := buildDecideTool([]string{"claude-code"}, nil, []string{"target-1"})
	raw, _ := json.Marshal(tool)
	for _, gone := range []string{"provision_command", `"path"`} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("decide tool schema still offers %s", gone)
		}
	}
}

// TestDecide_RunCommand: run_command (LOOM-72) carries a registered
// target_id and the command, passed through untouched.
func TestDecide_RunCommand(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
		"action":    "run_command",
		"target_id": "target-1",
		"command":   "hostname && uptime",
	}))
	m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := m.Decide(context.Background(), "run `hostname && uptime` on jet01", nil,
		[]router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "remote"}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := router.Decision{Action: router.ActionRunCommand, TargetID: "target-1", Command: "hostname && uptime"}
	if !reflect.DeepEqual(dec, want) {
		t.Errorf("Decide = %+v, want %+v", dec, want)
	}
}

// A run_command naming an unregistered target, or with no command, is a
// validation failure — escalated, never acted on.
func TestDecide_RunCommand_Invalid_Escalates(t *testing.T) {
	for _, args := range []map[string]any{
		{"action": "run_command", "target_id": "sc1", "command": "uptime"},
		{"action": "run_command", "target_id": "target-1", "command": "  "},
	} {
		primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, args))
		escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
			"action": "answer_directly", "direct_answer": "from escalation",
		}))
		escalation := tierFor(escalationSrv, "escalation-model")
		m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dec, err := m.Decide(context.Background(), "run it", nil, []router.TargetSnapshot{{ID: "target-1", Name: "jet01", Kind: "remote"}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if dec.DirectAnswer != "from escalation" {
			t.Errorf("args %v: Decide = %+v, want the escalation's answer", args, dec)
		}
	}
}

// The system prompt tells the model to copy commands verbatim and never
// compose one unprompted.
func TestDecideSystemPrompt_RunCommandRules(t *testing.T) {
	for _, want := range []string{"run_command", "exactly as the user wrote it", "never invent"} {
		if !strings.Contains(decideSystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestDecideSystemPrompt_Persona (LOOM-61): answer_directly replies speak
// as Loomux — what it is, that it's single-user, and that it never
// promises something it can't do — instead of as a generic chatbot.
func TestDecideSystemPrompt_Persona(t *testing.T) {
	for _, want := range []string{
		"Loomux dispatches chat messages to AI coding agents",
		"machines the user has registered",
		"single-user",
		"speak as Loomux",
		"never promise",
		"don't claim to have done",
	} {
		if !strings.Contains(decideSystemPrompt, want) {
			t.Errorf("system prompt missing persona text %q", want)
		}
	}
}

// TestDecide_NoTargets_TargetActionBecomesRegisterTargetAnswer (LOOM-68):
// with no registered targets, a model that picks provision_workspace or
// run_command anyway gets the register-a-target answer instead of a
// validation error — no escalation, nothing for the API to 500 on.
func TestDecide_NoTargets_TargetActionBecomesRegisterTargetAnswer(t *testing.T) {
	for _, args := range []map[string]any{
		{"action": "provision_workspace", "agent_type": "claude-code",
			"new_workspace": map[string]any{"name": "x", "target_id": "", "kind": "empty"}},
		{"action": "run_command", "target_id": "", "command": "uptime"},
	} {
		primarySrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, args))
		escalationSrv, escalationCalls := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{
			"action": "answer_directly", "direct_answer": "from escalation",
		}))
		escalation := tierFor(escalationSrv, "escalation-model")
		m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dec, err := m.Decide(context.Background(), "set up a repo and run uptime", nil, nil)
		if err != nil {
			t.Fatalf("args %v: Decide: %v", args, err)
		}
		want := router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: router.NoTargetsReply}
		if !reflect.DeepEqual(dec, want) {
			t.Errorf("args %v: Decide = %+v, want %+v", args, dec, want)
		}
		if n := atomic.LoadInt32(escalationCalls); n != 0 {
			t.Errorf("args %v: escalation called %d times, want 0", args, n)
		}
	}
}

// With no targets the prompt tells the model that nothing can be run or
// provisioned and where the user registers a target (LOOM-68).
func TestDecideUserPrompt_NoTargets_SaysToRegisterOne(t *testing.T) {
	prompt := decideUserPrompt("clone my repo", nil, nil, router.DispatchOptions{})
	for _, want := range []string{"No targets are registered", "POST /api/v1/targets", "Targets page"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if withTarget := decideUserPrompt("hi", nil, []router.TargetSnapshot{{ID: "t1", Name: "jet01", Kind: "local"}}, router.DispatchOptions{}); strings.Contains(withTarget, "No targets are registered") {
		t.Errorf("prompt with a target still says none are registered:\n%s", withTarget)
	}
}
