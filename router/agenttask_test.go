package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func TestAgentTask(t *testing.T) {
	for _, c := range []struct {
		name, message string
		routing       []string
		want          string
	}{
		{"no routing text", "add a README", nil, "add a README"},
		{"readiness run t12-b", "Use Claude Code in workspace api-server. The agent should add a /healthz endpoint.",
			[]string{"Use Claude Code in workspace api-server.", "The agent should"}, "add a /healthz endpoint."},
		{"inside the sentence", "use codex to add a README in api-server",
			[]string{"use codex to", "in api-server"}, "add a README"},
		{"case differs", "In web-dashboard, make the sidebar collapsible", []string{"in web-dashboard,"},
			"make the sidebar collapsible"},
		{"leading punctuation left over", "Use codex in api-server: add a README", []string{"Use codex in api-server"},
			"add a README"},
		{"lines kept", "In api-server:\nfix the test\n\n    go test ./...", []string{"In api-server:"},
			"fix the test\n\n    go test ./..."},
		{"part not in the message is ignored", "add a README", []string{"in billing"}, "add a README"},
		{"a part the model saw redacted leaves the value", "in api-server set the token to hunter2hunter2 in .env",
			[]string{"in api-server", "set the token to [redacted]"}, "set the token to hunter2hunter2 in .env"},
		{"all routing: message whole", "Use e2e-loom56 then.", []string{"Use e2e-loom56 then."}, "Use e2e-loom56 then."},
		{"blank parts", "add a README", []string{"", "  "}, "add a README"},
		{"whole words only", "fix the handler in api-server/cmd, in api-server", []string{"in api-server"},
			"fix the handler in api-server/cmd"},
		{"not inside a word", "use codex to add codexify", []string{"codex"}, "use to add codexify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := router.AgentTask(c.message, c.routing); got != c.want {
				t.Errorf("AgentTask(%q, %q) = %q, want %q", c.message, c.routing, got, c.want)
			}
		})
	}
}

// F11 in the LOOM-234 readiness run (t12-b, t8b): the agent was sent the
// message as typed, routing instructions and all, and answered about
// Loomux instead of doing the task. It is sent the task; the
// conversation keeps the user's words.
func TestDispatch_AgentGetsTaskWithoutRoutingText(t *testing.T) {
	h := newRouterHarness(t)
	const message = "Use Claude Code in workspace api-server. The agent should add a /healthz endpoint that returns 200."
	h.model.DecideFunc = func(ctx context.Context, msg string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: h.ws.ID, AgentType: "claude-code",
			RoutingText: []string{"Use Claude Code in workspace api-server.", "The agent should"}}, nil
	}
	var relayed string
	h.model.RelayInputFunc = func(ctx context.Context, in router.RelayInput) (router.RelayResult, error) {
		relayed = in.UserMessage
		return router.RelayResult{Reply: "added", Done: true}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", message); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	sent := strings.Join(append(h.exec.launchedCommands(), h.exec.sentKeys()...), "\n")
	if !strings.Contains(sent, "add a /healthz endpoint that returns 200.") {
		t.Errorf("the agent got %q, want the task", sent)
	}
	for _, routing := range []string{"Claude Code", "workspace api-server", "The agent should"} {
		if strings.Contains(sent, routing) {
			t.Errorf("the agent got %q, which still says %q", sent, routing)
		}
	}
	if relayed != message {
		t.Errorf("relay was given %q, want the user's message as typed", relayed)
	}
	msgs, _ := h.store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(msgs) == 0 || msgs[0].Content != message {
		t.Errorf("stored messages = %+v, want the user's message as typed", msgs)
	}
}

// A plan a target's policy made wait for a yes carries its routing text:
// the confirmed turn sends the agent the task too.
func TestDispatch_ConfirmedPlanKeepsRoutingTextOut(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	d := h.provisionDecision("codex")
	d.RoutingText = []string{"With codex in a new workspace on jet01,"}
	h.decide(d)
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "With codex in a new workspace on jet01, print the hostname and uptime"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := h.r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	sent := h.sentToAgent(t, "codex")
	if !strings.Contains(sent, "print the hostname and uptime") || strings.Contains(sent, "With codex") {
		t.Errorf("the agent got %q, want the task without its routing text", sent)
	}
}
