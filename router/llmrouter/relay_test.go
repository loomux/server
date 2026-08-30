package llmrouter

import (
	"context"
	"strings"
	"testing"
)

func TestRelay_HappyPath(t *testing.T) {
	srv, _ := newFakeServer(t, textHandler(t, "Agent finished the task."))

	m, err := New(Config{Primary: tierFor(srv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := m.Relay(context.Background(), "raw captured pane output")
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if out != "Agent finished the task." {
		t.Errorf("Relay = %q, want %q", out, "Agent finished the task.")
	}
}

func TestRelay_PrimaryTransportError_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))
	escalationSrv, _ := newFakeServer(t, textHandler(t, "from escalation"))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := m.Relay(context.Background(), "raw output")
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if out != "from escalation" {
		t.Errorf("Relay = %q, want %q", out, "from escalation")
	}
}

func TestRelay_PrimaryEmptyResponse_EscalatesToHealthyEscalation(t *testing.T) {
	primarySrv, _ := newFakeServer(t, textHandler(t, "   "))
	escalationSrv, _ := newFakeServer(t, textHandler(t, "from escalation"))

	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := m.Relay(context.Background(), "raw output")
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if out != "from escalation" {
		t.Errorf("Relay = %q, want %q", out, "from escalation")
	}
}

func TestRelay_PrimaryFails_NoEscalationConfigured_SurfacesDirectly(t *testing.T) {
	primarySrv, _ := newFakeServer(t, errorHandler(500))

	m, err := New(Config{Primary: tierFor(primarySrv, "test-model")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.Relay(context.Background(), "raw output")
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

	_, err = m.Relay(context.Background(), "raw output")
	if err == nil {
		t.Fatal("Relay: want error, got nil")
	}
	if !strings.Contains(err.Error(), "primary model failed") || !strings.Contains(err.Error(), "escalation model also failed") {
		t.Errorf("Relay err = %q, want it to mention both primary and escalation failures", err.Error())
	}
}
