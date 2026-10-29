package llmrouter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

// LOOM-258: the routing model quotes the parts of the message that only
// route it; they come back on the decision, blanks dropped, for the
// router to leave out of what the agent is sent.
func TestDecide_RoutingText(t *testing.T) {
	workspaces := []router.WorkspaceSnapshot{{ID: "ws-1", Name: "api-server"}}
	targets := []router.TargetSnapshot{{ID: "t-1", Name: "buildbox"}}
	for _, c := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"use_workspace", map[string]any{"action": "use_workspace", "workspace_id": "ws-1", "agent_type": "codex",
			"routing_text": []string{"Use codex in api-server.", " ", ""}}, []string{"Use codex in api-server."}},
		{"provision_workspace", map[string]any{"action": "provision_workspace", "agent_type": "codex",
			"new_workspace": map[string]any{"name": "scratch", "target_id": "t-1", "kind": "empty"},
			"routing_text":  []string{" in a new workspace on buildbox "}}, []string{"in a new workspace on buildbox"}},
		{"none given", map[string]any{"action": "use_workspace", "workspace_id": "ws-1", "agent_type": "codex"}, nil},
		{"not for an answer", map[string]any{"action": "answer_directly", "direct_answer": "hi",
			"routing_text": []string{"Use codex"}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newFakeServer(t, toolCallHandler(t, decideToolName, c.args))
			m, err := New(Config{Primary: tierFor(srv, "test-model")}, []string{"codex"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			dec, err := m.Decide(context.Background(), "Use codex in api-server. Add a README.", workspaces, targets)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if !reflect.DeepEqual(dec.RoutingText, c.want) {
				t.Errorf("RoutingText = %q, want %q", dec.RoutingText, c.want)
			}
		})
	}
}

func TestDecideTool_RoutingText(t *testing.T) {
	props := decideTool([]string{"codex"}, []string{"ws-1"}, []string{"t-1"}, false, nil).schema["properties"].(map[string]any)
	p, ok := props["routing_text"].(map[string]any)
	if !ok || p["type"] != "array" || !reflect.DeepEqual(p["items"], map[string]any{"type": "string"}) {
		t.Fatalf("routing_text = %#v, want an array of strings", props["routing_text"])
	}
	if d, _ := props["direct_answer"].(map[string]any)["description"].(string); !strings.Contains(d, "Never a request to confirm") {
		t.Errorf("direct_answer description = %q, want it to rule out asking for a yes", d)
	}
}

// The prompt's halves of LOOM-258 and LOOM-255: the agent gets the task
// without its routing text, and only Loomux asks for confirmation.
func TestDecideSystemPrompt_RoutingTextAndConfirmation(t *testing.T) {
	for _, want := range []string{
		"the agent is sent the message without its routing_text",
		"Never put any part of the task there",
		`Never ask the user to confirm anything in direct_answer, nor to reply "yes"`,
		"Asking for confirmation is Loomux's alone",
		"don't copy Loomux's own confirmation messages",
	} {
		if !strings.Contains(decideSystemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}
