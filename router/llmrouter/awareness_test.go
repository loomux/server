package llmrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/router"
)

// fixedNow pins "now" for relative times in the prompt.
func fixedNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })
}

// LOOM-88: each workspace's status, target, last use and recent summary
// reach the prompt.
func TestDecideUserPrompt_WorkspaceDetails(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)
	used := now.Add(-3 * time.Hour)
	prompt := decideUserPrompt("hi", []router.WorkspaceSnapshot{
		{ID: "ws-1", Name: "api-server", Status: "idle", TargetName: "jet01", LastUsed: &used, Summary: "added the rate limiter"},
		{ID: "ws-2", Name: "scratch", Status: "provisioning", TargetName: "sc1"},
	}, nil, router.DispatchOptions{})
	for _, want := range []string{
		"status: idle, on jet01, last used 3h ago",
		"recent: added the rate limiter",
		"status: provisioning, on sc1, never used",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt[strings.Index(prompt, "ws-2"):], "recent:") {
		t.Errorf("an empty summary rendered a recent: line:\n%s", prompt)
	}
}

func TestRelativeAge(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now",
		5 * time.Minute:  "5m ago",
		3 * time.Hour:    "3h ago",
		50 * time.Hour:   "2d ago",
	} {
		if got := relativeAge(now, now.Add(-d)); got != want {
			t.Errorf("relativeAge(-%v) = %q, want %q", d, got, want)
		}
	}
}

// LOOM-88: a probed agent's version is shown with its availability.
func TestDecideUserPrompt_AgentVersions(t *testing.T) {
	prompt := decideUserPrompt("hi", nil, []router.TargetSnapshot{{
		ID: "t1", Name: "jet01", Kind: "remote",
		Agents:        map[string]bool{"claude-code": true, "codex": false},
		AgentVersions: map[string]string{"claude-code": "2.1.4 (Claude Code)"},
	}}, router.DispatchOptions{})
	if want := "agents: claude-code: available (2.1.4 (Claude Code)), codex: not installed"; !strings.Contains(prompt, want) {
		t.Errorf("prompt missing %q:\n%s", want, prompt)
	}
}

// decideRequest captures the chat request the fake model receives.
type decideRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Parameters struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

func capturingDecideServer(t *testing.T, args map[string]any) (Tier, *decideRequest) {
	t.Helper()
	var got decideRequest
	handler := toolCallHandler(t, decideToolName, args)
	srv, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		handler(w, r)
	})
	return tierFor(srv, "test-model"), &got
}

// LOOM-88: the system prompt describes each agent type, so the model can
// tell claude-code from codex, and says when a plain command will do.
func TestDecide_SystemPromptDescribesAgents(t *testing.T) {
	tier, got := capturingDecideServer(t, map[string]any{"action": "answer_directly", "direct_answer": "ok"})
	m, err := New(Config{Primary: tier}, []string{"claude-code", "codex"}, WithAgentDescriptions(map[string]string{
		"claude-code": "Anthropic's Claude Code CLI.",
		"codex":       "OpenAI's Codex CLI.",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Decide(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	system := got.Messages[0].Content
	for _, want := range []string{
		"Agent types:\n- claude-code: Anthropic's Claude Code CLI.\n- codex: OpenAI's Codex CLI.",
		"plain shell command",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q:\n%s", want, system)
		}
	}
}
