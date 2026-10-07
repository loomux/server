package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-110: a turn that runs a command leaves its decision (with the
// model and tier), the command with credential values redacted, where it
// ran and how it ended, and the dispatch's outcome.
func TestDispatch_AuditTrail_RunCommand(t *testing.T) {
	h := newCommandHarness(t)
	ctx := context.Background()
	if err := h.store.CreateCredential(ctx, &registry.Credential{
		ID: uuid.NewString(), Name: "API_TOKEN", AgentType: "codex", Value: "tok-very-secret",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	command := "curl -H 'Authorization: tok-very-secret' localhost"
	h.decide(router.Decision{Action: router.ActionRunCommand, TargetID: h.target.ID, Command: command,
		Model: "router-model", Tier: "primary"})
	h.outputs[command] = "ok"

	if _, err := h.r.Dispatch(ctx, "conv-1", "run `"+command+"` on jet01"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	events, err := h.store.ListDispatchEventsByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatalf("ListDispatchEventsByConversation: %v", err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if strings.Contains(e.Command, "tok-very-secret") || strings.Contains(e.Detail, "tok-very-secret") {
			t.Errorf("event %s keeps a credential value: %+v", e.Kind, e)
		}
	}
	if got, want := strings.Join(kinds, ","), "decision,command,outcome"; got != want {
		t.Fatalf("event kinds = %s, want %s", got, want)
	}
	decision, cmd, outcome := events[0], events[1], events[2]
	if decision.Model != "router-model" || decision.Tier != "primary" || decision.Outcome != "run_command" ||
		decision.TargetID != h.target.ID {
		t.Errorf("decision event = %+v", decision)
	}
	if cmd.TargetID != h.target.ID || cmd.TaskID == "" || cmd.Outcome != "exit 0" || !strings.Contains(cmd.Command, "curl") {
		t.Errorf("command event = %+v, want target, task, exit 0 and the redacted command", cmd)
	}
	if outcome.Outcome != "success" || outcome.ErrorClass != "" {
		t.Errorf("outcome event = %+v, want success", outcome)
	}
}

// LOOM-110: an offer, and its answer, are in the trail too.
func TestDispatch_AuditTrail_Offer(t *testing.T) {
	h := newCommandHarness(t)
	ctx := context.Background()
	h.decideCommand("hostname && uptime")
	h.outputs["hostname && uptime"] = "jet01"

	if _, err := h.r.Dispatch(ctx, "conv-1", "how long has jet01 been up?"); err != nil {
		t.Fatalf("Dispatch (offer): %v", err)
	}
	if _, err := h.r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch (yes): %v", err)
	}
	events, _ := h.store.ListDispatchEventsByConversation(ctx, "conv-1")
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind+":"+e.Outcome)
	}
	got := strings.Join(kinds, ",")
	for _, want := range []string{"offer:run_command", "offer_answered:approved", "command:exit 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("events %s lack %s", got, want)
		}
	}
}
