package llmrouter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/router"
)

// sequenceHandler answers the i-th request with handlers[i] (the last
// one repeating) and keeps each request's body.
func sequenceHandler(handlers ...http.HandlerFunc) (http.HandlerFunc, func() []string) {
	var mu sync.Mutex
	var bodies []string
	return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			i := len(bodies)
			bodies = append(bodies, string(b))
			mu.Unlock()
			if i >= len(handlers) {
				i = len(handlers) - 1
			}
			handlers[i](w, r)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), bodies...)
		}
}

// An invented workspace id is fixed by one retry on the same tier that
// says what was wrong, without escalating (LOOM-107).
func TestDecide_InvalidID_RecoversByCorrectiveRetry(t *testing.T) {
	h, bodies := sequenceHandler(
		toolCallHandler(t, decideToolName, map[string]any{"action": "use_workspace", "workspace_id": "ws-invented", "agent_type": "claude-code"}),
		toolCallHandler(t, decideToolName, map[string]any{"action": "use_workspace", "workspace_id": "ws-1", "agent_type": "claude-code"}),
	)
	primarySrv, primaryCount := newFakeServer(t, h)
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}

	dec, err := m.Decide(context.Background(), "fix the bug", []router.WorkspaceSnapshot{{ID: "ws-1"}}, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Action != router.ActionUseWorkspace || dec.WorkspaceID != "ws-1" {
		t.Fatalf("Decide = %+v, want use_workspace ws-1", dec)
	}
	if *primaryCount != 2 || *escalationCount != 0 {
		t.Errorf("primary called %d times, escalation %d; want 2 and 0", *primaryCount, *escalationCount)
	}
	if b := bodies(); len(b) != 2 || !strings.Contains(b[1], "was rejected") || !strings.Contains(b[1], "ws-invented") {
		t.Errorf("the retry doesn't say what was wrong: %v", b)
	}
}

// A second invalid answer escalates, as before.
func TestDecide_InvalidTwice_Escalates(t *testing.T) {
	primarySrv, primaryCount := newFakeServer(t, malformedToolCallHandler(t, decideToolName))
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := m.Decide(context.Background(), "hi", nil, nil)
	if err != nil || dec.DirectAnswer != "from escalation" {
		t.Fatalf("Decide = %+v, %v", dec, err)
	}
	if *primaryCount != 2 || *escalationCount != 1 {
		t.Errorf("primary called %d times, escalation %d; want 2 and 1", *primaryCount, *escalationCount)
	}
}

// With the primary down, after breakerThreshold failures messages go
// straight to escalation for breakerCooldown, then the primary is tried
// again and, answering, takes over (LOOM-107).
func TestDecide_PrimaryBreaker(t *testing.T) {
	var primaryUp atomic.Bool
	answer := toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from primary"})
	primarySrv, primaryCount := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if primaryUp.Load() {
			answer(w, r)
			return
		}
		// 400: an HTTP failure the SDK doesn't retry, to keep this fast.
		errorHandler(http.StatusBadRequest)(w, r)
	})
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.primaryBreaker.now = func() time.Time { return now }

	decide := func() string {
		t.Helper()
		dec, err := m.Decide(context.Background(), "hi", nil, nil)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		return dec.DirectAnswer
	}
	for i := 0; i < breakerThreshold; i++ {
		decide()
	}
	if got := atomic.LoadInt32(primaryCount); got != breakerThreshold {
		t.Fatalf("primary called %d times before the breaker, want %d", got, breakerThreshold)
	}
	if got := decide(); got != "from escalation" {
		t.Fatalf("with the breaker open: %q", got)
	}
	if got := atomic.LoadInt32(primaryCount); got != breakerThreshold {
		t.Errorf("primary called while its breaker is open (%d calls)", got)
	}

	primaryUp.Store(true)
	now = now.Add(breakerCooldown)
	if got := decide(); got != "from primary" {
		t.Fatalf("after the cooldown: %q, want the primary to be tried again", got)
	}
	if got := decide(); got != "from primary" {
		t.Fatalf("after recovering: %q", got)
	}
	if got := atomic.LoadInt32(escalationCount); got != breakerThreshold+1 {
		t.Errorf("escalation called %d times, want %d", got, breakerThreshold+1)
	}
}

// Invalid answers don't trip the breaker: the provider is up.
func TestDecide_InvalidAnswersDontTripBreaker(t *testing.T) {
	primarySrv, _ := newFakeServer(t, malformedToolCallHandler(t, decideToolName))
	escalationSrv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < breakerThreshold+1; i++ {
		if _, err := m.Decide(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !m.primaryBreaker.allow() {
		t.Error("breaker opened on invalid answers")
	}
}

// Without an escalation tier the breaker never skips the primary: there
// would be nothing to answer instead.
func TestDecide_NoEscalation_NeverSkipsPrimary(t *testing.T) {
	primarySrv, primaryCount := newFakeServer(t, errorHandler(http.StatusBadRequest))
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < breakerThreshold+2; i++ {
		_, _ = m.Decide(context.Background(), "hi", nil, nil)
	}
	if got := atomic.LoadInt32(primaryCount); got != breakerThreshold+2 {
		t.Errorf("primary called %d times, want every time (%d)", got, breakerThreshold+2)
	}
}
