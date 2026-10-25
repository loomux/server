package router_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// stubMaker is a router.MachineMaker over one fixed offer: Plan resolves
// a spec against it as providers.Plan would, refusing trusted for a name
// not in trusted.
type stubMaker struct {
	mu      sync.Mutex
	offers  []router.MachineOffer
	trusted []string
	planned []router.MachineSpec
	created []router.MachinePlan
	// createErr, when set, is what Create returns instead of a target.
	createErr error
}

func newStubMaker() *stubMaker {
	return &stubMaker{offers: []router.MachineOffer{{
		PluginID: "plug-1", Plugin: "kubernetes", Label: "test cluster",
		Sizes: []router.MachineSize{
			{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"},
			{Name: "medium", CPU: "2", Memory: "4Gi", Disk: "20Gi", MemoryRequest: "1536Mi"},
		},
		PersistentDefault: true, Persistent: true, Ephemeral: true,
		Egress: []string{"internet", "none", "trusted"}, Machines: 1, MaxMachines: 5,
	}}}
}

func (m *stubMaker) Offers(ctx context.Context) ([]router.MachineOffer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.offers, nil
}

func (m *stubMaker) Plan(ctx context.Context, spec router.MachineSpec) (router.MachinePlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.planned = append(m.planned, spec)
	var offer *router.MachineOffer
	for i := range m.offers {
		if m.offers[i].PluginID == spec.PluginID {
			offer = &m.offers[i]
		}
	}
	if offer == nil {
		return router.MachinePlan{}, &router.MachineRefusal{Reason: "that plugin isn't installed, enabled and running."}
	}
	size := offer.Sizes[0]
	for _, s := range offer.Sizes {
		if s.Name == spec.Size {
			size = s
		}
	}
	persistent := offer.PersistentDefault
	if spec.Persistent != nil {
		persistent = *spec.Persistent
	}
	egress := spec.Egress
	if egress == "" {
		egress = "internet"
	}
	if egress == "trusted" && !contains(m.trusted, spec.Name) {
		return router.MachinePlan{}, &router.MachineRefusal{Reason: "egress trusted is only for the machines the plugin's trusted_machines names."}
	}
	return router.MachinePlan{PluginID: offer.PluginID, Plugin: offer.Plugin, Label: offer.Label, Name: spec.Name,
		Size: size, Persistent: persistent, Egress: egress, Machines: offer.Machines, MaxMachines: offer.MaxMachines}, nil
}

func (m *stubMaker) Create(ctx context.Context, plan router.MachinePlan) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return "", m.createErr
	}
	m.created = append(m.created, plan)
	return "target-new", nil
}

func (m *stubMaker) createdPlans() []router.MachinePlan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]router.MachinePlan(nil), m.created...)
}

// machineRouter is a Router with maker, whose model proposes d for any
// message and counts its calls.
func machineRouter(t *testing.T, maker router.MachineMaker, d router.Decision) (registry.Store, *router.Router, *int) {
	t.Helper()
	store := newTestStore(t)
	var opts []router.Option
	if maker != nil {
		opts = append(opts, router.WithMachineMaker(maker))
	}
	_, r, model := newRouter(t, store, opts...)
	calls := 0
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		calls++
		if message == "hello" {
			return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "hi"}, nil
		}
		return d, nil
	}
	return store, r, &calls
}

func proposal(name, size, egress string) router.Decision {
	return router.Decision{Action: router.ActionProvisionTarget,
		NewTarget: router.MachineSpec{PluginID: "plug-1", Name: name, Size: size, Egress: egress}}
}

func confirmations(t *testing.T, store registry.Store) []*registry.Confirmation {
	t.Helper()
	list, err := store.ListConfirmationsByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListConfirmationsByConversation: %v", err)
	}
	return list
}

// LOOM-216: the model's provision_target only proposes; the machine, as
// resolved from the plugin's offer, is shown and created only on a yes.
func TestProvisionTarget_OfferThenCreateOnYes(t *testing.T) {
	maker := newStubMaker()
	store, r, calls := machineRouter(t, maker, proposal("builds", "medium", ""))
	ctx := context.Background()

	reply, err := r.Dispatch(ctx, "conv-1", "make me a new machine for builds")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, want := range []string{"builds", "kubernetes (test cluster)", "medium: 2 CPU, 4Gi memory, 20Gi disk",
		"1536Mi memory", "kept", "network:  internet", "1 of at most 5", `Reply "yes" to create it`} {
		if !strings.Contains(reply, want) {
			t.Errorf("the offer doesn't say %q:\n%s", want, reply)
		}
	}
	if got := maker.createdPlans(); len(got) != 0 {
		t.Fatalf("created on the decision alone: %+v", got)
	}
	confs := confirmations(t, store)
	if len(confs) != 1 || confs[0].Kind != registry.ConfirmationCreateTarget || confs[0].TargetName != "builds" ||
		confs[0].Status != registry.ConfirmationPending || !strings.Contains(confs[0].Command, "size medium") {
		t.Fatalf("confirmations = %+v", confs)
	}

	reply, err = r.Dispatch(ctx, "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch yes: %v", err)
	}
	if !strings.Contains(reply, "Creating builds with the kubernetes (test cluster) plugin") {
		t.Errorf("reply = %q", reply)
	}
	got := maker.createdPlans()
	if len(got) != 1 || got[0].Name != "builds" || got[0].Size.Name != "medium" || !got[0].Persistent || got[0].Egress != "internet" {
		t.Fatalf("created = %+v", got)
	}
	if *calls != 1 {
		t.Errorf("the routing model was asked %d times; a yes isn't routed", *calls)
	}
	if confs := confirmations(t, store); confs[0].Status != registry.ConfirmationApproved || confs[0].StatusReason != "" {
		t.Errorf("confirmation = %+v", confs[0])
	}

	// One-shot: another yes creates nothing more.
	if _, err := r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 1 {
		t.Errorf("a second yes created again: %+v", got)
	}
}

// Anything but a confirmation cancels the offer, and is routed as usual.
func TestProvisionTarget_DeclinedCreatesNothing(t *testing.T) {
	maker := newStubMaker()
	store, r, _ := machineRouter(t, maker, proposal("builds", "", ""))
	ctx := context.Background()
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	reply, err := r.Dispatch(ctx, "conv-1", "hello")
	if err != nil || reply != "hi" {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	if _, err := r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 0 {
		t.Errorf("created after a decline: %+v", got)
	}
	if confs := confirmations(t, store); confs[0].Status != registry.ConfirmationDenied {
		t.Errorf("confirmation = %+v", confs[0])
	}
}

// The routing model is given the offers to choose from.
func TestProvisionTarget_OffersReachTheModel(t *testing.T) {
	maker := newStubMaker()
	store := newTestStore(t)
	_, r, model := newRouter(t, store, router.WithMachineMaker(maker))
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "what can you do"); err != nil {
		t.Fatal(err)
	}
	if got := model.LastDecideOptions.Machines; len(got) != 1 || got[0].PluginID != "plug-1" || len(got[0].Sizes) != 2 {
		t.Errorf("machines given to the model = %+v", got)
	}
}

// Without a plugin that makes machines, a proposal is answered with how
// to get one, and nothing is planned.
func TestProvisionTarget_NoPlugin(t *testing.T) {
	for name, maker := range map[string]*stubMaker{"no maker": nil, "no offers": {}} {
		t.Run(name, func(t *testing.T) {
			var m router.MachineMaker
			if maker != nil {
				m = maker
			}
			store, r, _ := machineRouter(t, m, proposal("builds", "", ""))
			reply, err := r.Dispatch(context.Background(), "conv-1", "a new machine please")
			if err != nil || reply != router.NoMachinePluginReply {
				t.Errorf("reply = %q, %v", reply, err)
			}
			if maker != nil && len(maker.planned) != 0 {
				t.Errorf("planned without an offer: %+v", maker.planned)
			}
			if confs := confirmations(t, store); len(confs) != 0 {
				t.Errorf("an offer was made: %+v", confs)
			}
		})
	}
}

// Egress trusted is offered only for a name the plugin allows it; for
// any other the user is told why and nothing awaits a yes.
func TestProvisionTarget_TrustedOnlyForAllowedNames(t *testing.T) {
	maker := newStubMaker()
	maker.trusted = []string{"cc"}
	store, r, _ := machineRouter(t, maker, proposal("builds", "", "trusted"))
	ctx := context.Background()
	reply, err := r.Dispatch(ctx, "conv-1", "a machine that can reach the LAN")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "I can't create that machine: egress trusted is only for") {
		t.Errorf("reply = %q", reply)
	}
	if confs := confirmations(t, store); len(confs) != 0 {
		t.Errorf("a trusted machine was offered for a name not allowed it: %+v", confs)
	}
	if _, err := r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 0 {
		t.Errorf("created = %+v", got)
	}

	// The allowed name is offered, saying what trusted opens.
	store, r, _ = machineRouter(t, maker, proposal("cc", "", "trusted"))
	reply, err = r.Dispatch(ctx, "conv-1", "the hub machine, on the LAN")
	if err != nil || !strings.Contains(reply, "trusted: the LAN and the tailnet as well as the internet") {
		t.Errorf("reply = %q, %v", reply, err)
	}
	if confs := confirmations(t, store); len(confs) != 1 || !strings.Contains(confs[0].Command, "egress trusted") {
		t.Errorf("confirmations = %+v", confs)
	}
}

// trustedPlanner is a maker whose Plan answers trusted however it was
// asked: the router still never offers trusted it didn't ask for.
type trustedPlanner struct{ *stubMaker }

func (m trustedPlanner) Plan(ctx context.Context, spec router.MachineSpec) (router.MachinePlan, error) {
	p, err := m.stubMaker.Plan(ctx, spec)
	p.Egress = "trusted"
	return p, err
}

func TestProvisionTarget_NeverTrustedUnasked(t *testing.T) {
	store, r, _ := machineRouter(t, trustedPlanner{newStubMaker()}, proposal("builds", "", ""))
	reply, err := r.Dispatch(context.Background(), "conv-1", "a new machine please")
	if err != nil || !strings.Contains(reply, "only ever made when asked for") {
		t.Errorf("reply = %q, %v", reply, err)
	}
	if confs := confirmations(t, store); len(confs) != 0 {
		t.Errorf("confirmations = %+v", confs)
	}
}

// A yes creates only what the offer showed: if the plugin's offer has
// changed meanwhile, nothing is created.
func TestProvisionTarget_ChangedOfferCreatesNothing(t *testing.T) {
	maker := newStubMaker()
	_, r, _ := machineRouter(t, maker, proposal("builds", "medium", ""))
	ctx := context.Background()
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	maker.mu.Lock()
	maker.offers[0].Sizes[1].Memory = "8Gi"
	maker.offers[0].Machines = 3 // a count changing alone is no change
	maker.mu.Unlock()
	reply, err := r.Dispatch(ctx, "conv-1", "yes")
	if err != nil || !strings.Contains(reply, "the plugin's offer changed since I asked") {
		t.Errorf("reply = %q, %v", reply, err)
	}
	if got := maker.createdPlans(); len(got) != 0 {
		t.Errorf("created = %+v", got)
	}
}

// A yes refused afterwards leaves the offer's card denied, with the
// reason the chat was told, never approved: nothing was created.
func TestProvisionTarget_RefusedYesIsNotApproved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(store registry.Store, maker *stubMaker)
		reason string
	}{
		{"offer changed", func(store registry.Store, maker *stubMaker) {
			maker.mu.Lock()
			maker.offers[0].Sizes[0].Memory = "8Gi"
			maker.mu.Unlock()
		}, "the plugin's offer changed since I asked"},
		{"name taken meanwhile", func(store registry.Store, maker *stubMaker) {
			if err := store.CreateTarget(context.Background(), &registry.Target{ID: "t-1", Name: "builds", Kind: registry.TargetKindLocal}); err != nil {
				t.Fatal(err)
			}
		}, "a target named builds was registered meanwhile."},
		{"quota", func(store registry.Store, maker *stubMaker) {
			maker.createErr = &router.MachineRefusal{Reason: "the plugin's machine limit is reached; delete a machine it made first."}
		}, "the plugin's machine limit is reached; delete a machine it made first."},
		{"plugin gone", func(store registry.Store, maker *stubMaker) {
			maker.mu.Lock()
			maker.offers = nil
			maker.mu.Unlock()
		}, "that plugin isn't installed, enabled and running."},
		{"create failed", func(store registry.Store, maker *stubMaker) {
			maker.createErr = errors.New("database is locked")
		}, "creating it failed in Loomux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			maker := newStubMaker()
			store, r, _ := machineRouter(t, maker, proposal("builds", "", ""))
			ctx := context.Background()
			if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
				t.Fatal(err)
			}
			tc.before(store, maker)
			reply, err := r.Dispatch(ctx, "conv-1", "yes")
			if err == nil && !strings.Contains(reply, "I didn't create builds") {
				t.Errorf("reply = %q", reply)
			}
			confs := confirmations(t, store)
			if len(confs) != 1 || confs[0].Status != registry.ConfirmationDenied || !strings.Contains(confs[0].StatusReason, tc.reason) {
				t.Errorf("confirmation = %+v, want denied with %q", confs, tc.reason)
			}
		})
	}
}

// A machine count changing between offer and yes still creates it.
func TestProvisionTarget_CountChangeStillCreates(t *testing.T) {
	maker := newStubMaker()
	_, r, _ := machineRouter(t, maker, proposal("builds", "", ""))
	ctx := context.Background()
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	maker.mu.Lock()
	maker.offers[0].Machines = 3
	maker.mu.Unlock()
	if _, err := r.Dispatch(ctx, "conv-1", "create it"); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 1 {
		t.Errorf("created = %+v", got)
	}
}

// A name that isn't a slug, or that a target already has, is refused
// before the plugin is asked.
func TestProvisionTarget_NameRefusals(t *testing.T) {
	ctx := context.Background()
	maker := newStubMaker()
	_, r, _ := machineRouter(t, maker, proposal("Bad Name", "", ""))
	reply, err := r.Dispatch(ctx, "conv-1", "a new machine please")
	if err != nil || !strings.Contains(reply, "its name must be lowercase") {
		t.Errorf("reply = %q, %v", reply, err)
	}

	store, r, _ := machineRouter(t, maker, proposal("jet01", "", ""))
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t-1", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatal(err)
	}
	reply, err = r.Dispatch(ctx, "conv-1", "a new machine please")
	if err != nil || !strings.Contains(reply, "a target named jet01 already exists") {
		t.Errorf("reply = %q, %v", reply, err)
	}
	if len(maker.planned) != 0 {
		t.Errorf("the plugin was asked about a refused name: %+v", maker.planned)
	}
}

// A new machine is a deliberate choice: an open task doesn't swallow it.
func TestApplyAffinity_KeepsProvisionTarget(t *testing.T) {
	d := proposal("builds", "", "")
	out, from := router.ApplyAffinity(d, &router.OpenTaskSnapshot{TaskID: "task-1", WorkspaceID: "ws-1", AgentType: "claude-code"})
	if from != "" || out.Action != router.ActionProvisionTarget {
		t.Errorf("affinity turned provision_target into %+v (from %q)", out, from)
	}
}

// Nor does it when no plugin can make the machine: the answer saying so
// isn't typed into the open task's pane.
func TestProvisionTarget_NoPluginLeavesOpenTask(t *testing.T) {
	h, task := openTaskHarness(t)
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return proposal("builds", "", ""), nil
	}
	before := len(h.exec.sessionFor(task.TmuxSession).keys)
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "make me a new machine")
	if err != nil || reply != router.NoMachinePluginReply {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	if after := len(h.exec.sessionFor(task.TmuxSession).keys); after != before {
		t.Error("the request for a machine was typed into the open task's pane")
	}
}

// A create_target offer's card answers it as a typed reply does: Approve
// creates it once, without routing; Deny creates nothing.
func TestProvisionTarget_CardApproveAndDeny(t *testing.T) {
	ctx := context.Background()
	t.Run("approve", func(t *testing.T) {
		maker := newStubMaker()
		store, r, calls := machineRouter(t, maker, proposal("builds", "", ""))
		if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
			t.Fatal(err)
		}
		c := confirmations(t, store)[0]
		reply, err := r.Dispatch(ctx, "conv-1", "yes", router.WithConfirmationID(c.ID))
		if err != nil || !strings.Contains(reply, "Creating builds") {
			t.Fatalf("reply = %q, %v", reply, err)
		}
		if got := maker.createdPlans(); len(got) != 1 || *calls != 1 {
			t.Fatalf("created = %+v, routed %d times", got, *calls)
		}
		if got := confirmations(t, store)[0]; got.Status != registry.ConfirmationApproved {
			t.Errorf("after approve: %+v", got)
		}
		// A second click creates nothing more and isn't routed.
		reply, err = r.Dispatch(ctx, "conv-1", "yes", router.WithConfirmationID(c.ID))
		if err != nil || !strings.Contains(reply, "no longer waiting") {
			t.Errorf("second approve: %q, %v", reply, err)
		}
		if got := maker.createdPlans(); len(got) != 1 || *calls != 1 {
			t.Errorf("second approve: created %+v, routed %d times", got, *calls)
		}
	})
	t.Run("deny", func(t *testing.T) {
		maker := newStubMaker()
		store, r, _ := machineRouter(t, maker, proposal("builds", "", ""))
		if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
			t.Fatal(err)
		}
		c := confirmations(t, store)[0]
		if _, err := r.Dispatch(ctx, "conv-1", "no", router.WithConfirmationID(c.ID)); err != nil {
			t.Fatal(err)
		}
		if got := maker.createdPlans(); len(got) != 0 {
			t.Errorf("created after a deny: %+v", got)
		}
		if got := confirmations(t, store)[0]; got.Status != registry.ConfirmationDenied {
			t.Errorf("after deny: %+v", got)
		}
	})
}

// The card of an offer a newer one replaced creates nothing, and leaves
// the newer offer awaiting its answer.
func TestProvisionTarget_StaleCardCreatesNothing(t *testing.T) {
	ctx := context.Background()
	maker := newStubMaker()
	store, r, calls := machineRouter(t, maker, proposal("builds", "", ""))
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Dispatch(ctx, "conv-1", "a bigger one, actually"); err != nil {
		t.Fatal(err)
	}
	confs := confirmations(t, store)
	if len(confs) != 2 || confs[0].Status != registry.ConfirmationDenied || confs[1].Status != registry.ConfirmationPending {
		t.Fatalf("confirmations = %+v", confs)
	}
	routed := *calls
	reply, err := r.Dispatch(ctx, "conv-1", "yes", router.WithConfirmationID(confs[0].ID))
	if err != nil || !strings.Contains(reply, "no longer waiting") {
		t.Fatalf("stale approve: %q, %v", reply, err)
	}
	if got := maker.createdPlans(); len(got) != 0 || *calls != routed {
		t.Fatalf("stale approve: created %+v, routed %v", got, *calls != routed)
	}
	if _, err := r.Dispatch(ctx, "conv-1", "yes", router.WithConfirmationID(confs[1].ID)); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 1 {
		t.Errorf("the live offer's approve created %+v, want one machine", got)
	}
}

// A yes after the offer expired creates nothing: the offer is recorded
// expired, and the yes is routed as any other message.
func TestProvisionTarget_ExpiredYesCreatesNothing(t *testing.T) {
	ctx := context.Background()
	maker := newStubMaker()
	store, r, calls := machineRouter(t, maker, proposal("builds", "", ""))
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	router.ExpireOffer(r, "conv-1")
	if _, err := r.Dispatch(ctx, "conv-1", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := maker.createdPlans(); len(got) != 0 {
		t.Errorf("created on an expired offer: %+v", got)
	}
	if *calls != 2 {
		t.Errorf("the routing model was asked %d times; the late yes is routed", *calls)
	}
	if got := confirmations(t, store)[0]; got.Status != registry.ConfirmationExpired {
		t.Errorf("confirmation = %+v, want expired", got)
	}
}

// A Create that fails in Loomux, not a refusal, fails the turn: an error
// rather than a reply, the dispatch recorded as a failure, and the offer
// denied with a reason pointing at the server log.
func TestProvisionTarget_CreateErrorFailsTheTurn(t *testing.T) {
	ctx := context.Background()
	maker := newStubMaker()
	store, r, _ := machineRouter(t, maker, proposal("builds", "", ""))
	if _, err := r.Dispatch(ctx, "conv-1", "a new machine please"); err != nil {
		t.Fatal(err)
	}
	maker.createErr = errors.New("database is locked")
	reply, err := r.Dispatch(ctx, "conv-1", "yes")
	if err == nil || !strings.Contains(err.Error(), "database is locked") || reply != "" {
		t.Fatalf("reply = %q, err = %v; want the error, no reply", reply, err)
	}
	events, err := store.ListDispatchEventsByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, e := range events {
		if e.Kind == registry.EventOutcome {
			outcomes = append(outcomes, e.Outcome+" "+e.Detail)
		}
	}
	if n := len(outcomes); n != 2 || outcomes[1] != "failure create_target" {
		t.Errorf("dispatch outcomes = %q, want the yes's to be failure create_target", outcomes)
	}
	c := confirmations(t, store)[0]
	if c.Status != registry.ConfirmationDenied || !strings.Contains(c.StatusReason, "the server log says why") {
		t.Errorf("confirmation = %+v", c)
	}
}
