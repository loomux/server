package router_test

import (
	"context"
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
	if confs := confirmations(t, store); confs[0].Status != registry.ConfirmationApproved {
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
