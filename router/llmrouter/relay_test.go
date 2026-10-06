package llmrouter

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

func TestRelay_HappyPath_Done(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "Agent finished the task.",
		"done":  true,
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := m.Relay(context.Background(), router.RelayInput{Captured: "raw captured pane output"})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if result.Reply != "Agent finished the task." || !result.Done {
		t.Errorf("Relay = %+v, want {Reply: %q, Done: true}", result, "Agent finished the task.")
	}
}

func TestRelay_HappyPath_NotDone(t *testing.T) {
	srv, _ := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "Agent is waiting on a clarifying question.",
		"done":  false,
	}))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := m.Relay(context.Background(), router.RelayInput{Captured: "raw captured pane output"})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if result.Reply != "Agent is waiting on a clarifying question." || result.Done {
		t.Errorf("Relay = %+v, want Done: false", result)
	}
}

func TestRelay_MalformedPrimary_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, primaryCount := newFakeServer(t, malformedToolCallHandler(t, relayToolName))
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "from escalation",
		"done":  true,
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if result.Reply != "from escalation" {
		t.Errorf("Relay.Reply = %q, want %q", result.Reply, "from escalation")
	}
	if *primaryCount == 0 || *escalationCount == 0 {
		t.Error("expected both primary and escalation servers to be called")
	}
}

func TestRelay_NoToolCall_TreatedAsFailure(t *testing.T) {
	srv, _ := newFakeServer(t, noToolCallHandler(t))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err == nil {
		t.Fatal("Relay: want error, got nil")
	}
}

func TestRelay_PrimaryTransportError_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "from escalation",
		"done":  true,
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if result.Reply != "from escalation" {
		t.Errorf("Relay.Reply = %q, want %q", result.Reply, "from escalation")
	}
}

func TestRelay_PrimaryEmptyReply_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, _ := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "   ",
		"done":  true,
	}))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, relayToolName, map[string]any{
		"reply": "from escalation",
		"done":  true,
	}))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if result.Reply != "from escalation" {
		t.Errorf("Relay.Reply = %q, want %q", result.Reply, "from escalation")
	}
}

func TestRelay_PrimaryFails_NoEscalationConfigured_SurfacesDirectly(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))

	m, err := New(Config{Primary: tierFor(primarySrv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err == nil {
		t.Fatal("Relay: want error, got nil")
	}
	if strings.Contains(err.Error(), "escalation") {
		t.Errorf("Relay err = %q, want no mention of escalation since none is configured", err.Error())
	}
}

func TestRelay_BothTiersFail_ReturnsCombinedError(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))
	escalationSrv, _ := newFakeServer(t, errorHandler(500))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Relay(context.Background(), router.RelayInput{Captured: "raw output"})
	if err == nil {
		t.Fatal("Relay: want error, got nil")
	}
	if !strings.Contains(err.Error(), "primary model failed") || !strings.Contains(err.Error(), "escalation model also failed") {
		t.Errorf("Relay err = %q, want it to mention both primary and escalation failures", err.Error())
	}
}

// LOOM-112: the relay model is told what the turn was for, with the
// captured output last.
func TestRelayUserPrompt(t *testing.T) {
	got := relayUserPrompt(router.RelayInput{
		UserMessage: "add a health endpoint", PreviousSummary: "Set up the HTTP server.",
		AgentType: "claude-code", Captured: "wrote health.go\ntests pass",
	})
	for _, want := range []string{"Agent: claude-code", "add a health endpoint", "Set up the HTTP server."} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "Captured output:\nwrote health.go\ntests pass") {
		t.Errorf("prompt doesn't end with the captured output:\n%s", got)
	}

	late := relayUserPrompt(router.RelayInput{Captured: "build finished"})
	if !strings.Contains(late, "after its turn had ended") || strings.Contains(late, "summary before") {
		t.Errorf("late-output prompt = %q, want it to say no message started it, and no summary section", late)
	}
}
