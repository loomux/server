package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func (h *commandHarness) confirmations(t *testing.T) []*registry.Confirmation {
	t.Helper()
	list, err := h.store.ListConfirmationsByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListConfirmationsByConversation: %v", err)
	}
	return list
}

// offerCommand makes conv-1's turn end on an offer to run a command the
// user didn't write out, and returns its record.
func (h *commandHarness) offerCommand(t *testing.T) *registry.Confirmation {
	t.Helper()
	h.decideCommand("df -h")
	h.outputs["df -h"] = "plenty"
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "how much disk is free on jet01?"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	confs := h.confirmations(t)
	if len(confs) != 1 {
		t.Fatalf("confirmations = %+v, want the offer recorded", confs)
	}
	return confs[0]
}

// LOOM-123: an offer is recorded for its card — what it would run, and
// where — and its Approve runs it once.
func TestConfirmation_ApproveFromTheCard(t *testing.T) {
	h := newCommandHarness(t)
	c := h.offerCommand(t)
	if c.Kind != registry.ConfirmationRunCommand || c.Command != "df -h" || c.TargetName != "jet01" ||
		c.TargetID != h.target.ID || c.Status != registry.ConfirmationPending || c.ExpiresAt.IsZero() {
		t.Fatalf("offer = %+v", c)
	}

	decides := h.decides
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes", router.WithConfirmationID(c.ID))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "exit 0") || len(h.commandTasks(t)) != 1 || h.decides != decides {
		t.Fatalf("approve: reply %q, %d commands, routed %v", reply, len(h.commandTasks(t)), h.decides != decides)
	}
	if got := h.confirmations(t)[0]; got.Status != registry.ConfirmationApproved || got.ResolvedAt == nil {
		t.Errorf("after approve: %+v", got)
	}

	// Approving again (a double click, a second tab) runs nothing and
	// isn't routed.
	reply, err = h.r.Dispatch(context.Background(), "conv-1", "yes", router.WithConfirmationID(c.ID))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "no longer waiting") || len(h.commandTasks(t)) != 1 || h.decides != decides {
		t.Errorf("second approve: reply %q, %d commands, routed %v", reply, len(h.commandTasks(t)), h.decides != decides)
	}
}

func TestConfirmation_DenyFromTheCard(t *testing.T) {
	h := newCommandHarness(t)
	c := h.offerCommand(t)
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "no", router.WithConfirmationID(c.ID)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(h.exec.launchedCommands()) != 0 {
		t.Fatal("a denied command ran")
	}
	if got := h.confirmations(t)[0]; got.Status != registry.ConfirmationDenied {
		t.Errorf("after deny: %+v", got)
	}
}

// Typing still works, and anything else cancels the offer.
func TestConfirmation_TypedAnswers(t *testing.T) {
	t.Run("yes", func(t *testing.T) {
		h := newCommandHarness(t)
		h.offerCommand(t)
		if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if got := h.confirmations(t)[0]; got.Status != registry.ConfirmationApproved || len(h.commandTasks(t)) != 1 {
			t.Errorf("typed yes: %+v, %d commands", got, len(h.commandTasks(t)))
		}
	})
	t.Run("something else", func(t *testing.T) {
		h := newCommandHarness(t)
		h.offerCommand(t)
		h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "sure"})
		if _, err := h.r.Dispatch(context.Background(), "conv-1", "actually, what time is it?"); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if got := h.confirmations(t)[0]; got.Status != registry.ConfirmationDenied || len(h.exec.launchedCommands()) != 0 {
			t.Errorf("other message: %+v", got)
		}
	})
}

// A card for an older offer, clicked after a newer offer replaced it,
// runs nothing, isn't routed, and leaves the newer offer awaiting its
// answer.
func TestConfirmation_StaleCardLeavesTheLiveOffer(t *testing.T) {
	h := newCommandHarness(t)
	old := h.offerCommand(t)
	h.decideCommand("uptime")
	h.outputs["uptime"] = "up"
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "and the load there?"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	confs := h.confirmations(t)
	if len(confs) != 2 || confs[0].Status != registry.ConfirmationDenied || confs[1].Status != registry.ConfirmationPending {
		t.Fatalf("confirmations = %+v %+v", confs[0], confs[1])
	}

	decides := h.decides
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes", router.WithConfirmationID(old.ID))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "Nothing was run") || len(h.exec.launchedCommands()) != 0 || h.decides != decides {
		t.Fatalf("stale approve: reply %q, ran %v", reply, h.exec.launchedCommands())
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes", router.WithConfirmationID(confs[1].ID)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 1 || !strings.Contains(cmds[0], "uptime") {
		t.Errorf("the live offer's approve ran %v, want uptime once", cmds)
	}
}
