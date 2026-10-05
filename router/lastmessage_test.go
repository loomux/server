package router_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// lastMessageHarness dispatches to a TierMarker claude-code whose hook
// payload (LOOM-91) is whatever payload holds, recording what each turn
// relays and every RunOnce command.
type lastMessageHarness struct {
	r        *router.Router
	store    registry.Store
	exec     *fakeExecutor
	ws       *registry.Workspace
	mu       sync.Mutex
	payload  string
	relayed  []string
	commands []string
	done     bool
	relayErr error
}

func newLastMessageHarness(t *testing.T) *lastMessageHarness {
	t.Helper()
	h := &lastMessageHarness{}
	store := newTestStore(t)
	h.store = store
	h.ws = createFixtureWorkspace(t, store)
	h.exec = newFakeExecutor()
	h.exec.fileExists = true // the hook has touched the marker
	h.exec.capture = "SCREEN: the last screenful only"
	h.exec.runOnce = func(command string) (string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.commands = append(h.commands, command)
		if strings.Contains(command, ".reply") && strings.Contains(command, "head -c") {
			p := h.payload
			if strings.Contains(command, "rm -f") {
				h.payload = "" // read once, then removed
			}
			return p, nil
		}
		if strings.Contains(command, ".relaying") && strings.Contains(command, "mv -f") {
			h.exec.mu.Lock()
			defer h.exec.mu.Unlock()
			if h.exec.fileExists {
				return "claimed\n", nil
			}
			return "", nil
		}
		if strings.Contains(command, "capture-pane") {
			return "PANE HISTORY with sk-live-secret-123456 in it\n", nil
		}
		return "", nil
	}
	markerDir := t.TempDir()
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle}},
		"claude-code": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierMarker},
			LaunchTemplate: "claude", LastMessageKey: "last_assistant_message"},
	}
	detector := completion.NewDetector(store, h.exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, h.exec.factory(), detector)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "claude-code"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.relayed = append(h.relayed, captured)
			if h.relayErr != nil {
				return router.RelayResult{}, h.relayErr
			}
			return router.RelayResult{Reply: "relayed", Done: h.done}, nil
		},
	}
	h.r = router.New(store, orch, h.exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	return h
}

func hookPayload(t *testing.T, key, message string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hook_event_name": "Stop", key: message})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The relay gets the agent's own final message, whole — 200 lines of it,
// not the last screenful.
func TestDispatch_RelaysAgentsLastMessage(t *testing.T) {
	h := newLastMessageHarness(t)
	var lines []string
	for i := 1; i <= 200; i++ {
		lines = append(lines, fmt.Sprintf("line %d of the answer", i))
	}
	answer := strings.Join(lines, "\n")
	h.payload = hookPayload(t, "last_assistant_message", answer)

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(h.relayed) != 1 || h.relayed[0] != answer {
		t.Fatalf("relayed %q, want the agent's whole last message", h.relayed)
	}
}

// No payload, an unreadable one, or one without the message: the pane is
// captured as before.
func TestDispatch_LastMessageFallsBackToPane(t *testing.T) {
	for name, payload := range map[string]string{
		"no payload":     "",
		"not json":       "{truncated",
		"no message key": `{"hook_event_name":"Stop"}`,
		"empty message":  `{"last_assistant_message":"   "}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newLastMessageHarness(t)
			h.payload = payload
			if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if len(h.relayed) != 1 || h.relayed[0] != h.exec.capture {
				t.Fatalf("relayed %q, want the pane capture", h.relayed)
			}
		})
	}
}

// A huge message is bounded before it reaches the relay model.
func TestDispatch_LastMessageBounded(t *testing.T) {
	h := newLastMessageHarness(t)
	h.payload = hookPayload(t, "last_assistant_message", "start\n"+strings.Repeat("x", 200_000))
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := h.relayed[0]
	if len(got) > router.MaxRelayInput+100 || !strings.HasPrefix(got, "start\n") || !strings.Contains(got, "truncated") {
		t.Fatalf("relayed %d bytes (prefix %q), want at most ~%d, the start kept and the cut marked",
			len(got), got[:min(len(got), 20)], router.MaxRelayInput)
	}
}

// Before a follow-up turn is sent into a live pane, a marker and payload
// left over from the last one are cleared: a turn that finished after it
// was given up on mustn't end — or answer — the next one.
func TestDispatch_FollowUpClearsStaleMarkerAndPayload(t *testing.T) {
	h := newLastMessageHarness(t)
	h.payload = hookPayload(t, "last_assistant_message", "first answer")
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	h.mu.Lock()
	h.commands = nil
	h.payload = hookPayload(t, "last_assistant_message", "second answer")
	h.mu.Unlock()
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "more"); err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	cleared := false
	for _, c := range h.commands {
		if strings.Contains(c, "rm -f") && strings.Contains(c, ".done") && !strings.Contains(c, "head -c") {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("no stale marker/payload cleared before the follow-up; commands: %q", h.commands)
	}
	if len(h.relayed) != 2 || h.relayed[1] != "second answer" {
		t.Errorf("relayed %q, want the second turn's own message", h.relayed)
	}
}

// LOOM-91: each turn is kept — what was sent, the agent's own message and
// the pane's scrollback — with credentials redacted, after the task ends.
func TestDispatch_RecordsTurnTranscript(t *testing.T) {
	h := newLastMessageHarness(t)
	ctx := context.Background()
	if err := h.store.CreateCredential(ctx, &registry.Credential{ID: "c1", Name: "KEY", Value: "sk-live-secret-123456"}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	h.done = true
	h.payload = hookPayload(t, "last_assistant_message", "the answer, quoting sk-live-secret-123456")
	if _, err := h.r.Dispatch(ctx, "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tasks, err := h.store.ListTasksByWorkspace(ctx, h.ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v", tasks, err)
	}
	turns, err := h.store.ListTaskTurns(ctx, tasks[0].ID)
	if err != nil {
		t.Fatalf("ListTaskTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %+v, want 1", turns)
	}
	turn := turns[0]
	if turn.UserMessage != "go" || !strings.HasPrefix(turn.AgentMessage, "the answer") || !strings.HasPrefix(turn.Pane, "PANE HISTORY") {
		t.Errorf("turn = %+v", turn)
	}
	if strings.Contains(turn.AgentMessage+turn.Pane, "sk-live-secret-123456") {
		t.Errorf("a credential was stored unredacted: %+v", turn)
	}
}

// A long final message is stored up to the transcript's own 256 KiB cap,
// not the relay's 64 KiB, and the cut never splits a rune.
func TestDispatch_RecordsLongTurnMessageRuneSafe(t *testing.T) {
	h := newLastMessageHarness(t)
	ctx := context.Background()
	h.done = true
	h.payload = hookPayload(t, "last_assistant_message", "start\n"+strings.Repeat("é", 150_000))
	if _, err := h.r.Dispatch(ctx, "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tasks, err := h.store.ListTasksByWorkspace(ctx, h.ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v", tasks, err)
	}
	turns, err := h.store.ListTaskTurns(ctx, tasks[0].ID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns = %+v, %v", turns, err)
	}
	msg := turns[0].AgentMessage
	if len(msg) < 200<<10 || len(msg) > 256<<10+100 {
		t.Errorf("stored %d bytes, want about 256 KiB", len(msg))
	}
	if !utf8.ValidString(msg) || !strings.HasPrefix(msg, "start\n") || !strings.Contains(msg, "truncated") {
		t.Errorf("stored message is not a valid, marked cut of the start (%d bytes)", len(msg))
	}
}
