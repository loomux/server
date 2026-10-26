package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
)

// MachineSize is one size a plugin offers, as its targets.describe
// gives it: the machine's limits, and the requests below them of a
// Burstable size (LOOM-241), empty when they equal the limits.
type MachineSize struct {
	Name          string
	CPU           string
	Memory        string
	Disk          string
	CPURequest    string
	MemoryRequest string
}

// MachineOffer is a plugin instance that can create a machine now
// (LOOM-216), as the routing model is shown it: its id, label and kind
// and what it offers. Never its configuration.
type MachineOffer struct {
	PluginID string
	// Plugin is the plugin's name (kubernetes, docker); Label the
	// instance's.
	Plugin string
	Label  string
	Sizes  []MachineSize
	// PersistentDefault is whether a machine keeps its data unless asked
	// otherwise; Persistent and Ephemeral which kinds the plugin makes.
	PersistentDefault bool
	Persistent        bool
	Ephemeral         bool
	// Egress is the network choices it offers. trusted among them only
	// says that some machine names may have it.
	Egress []string
	// Machines is how many it has made; MaxMachines its limit, 0 for none.
	Machines    int
	MaxMachines int
}

// MachineSpec is the routing model's proposal for a new machine
// (LOOM-216): names and choices from an offer, nothing else. Loomux
// resolves it against the plugin's offer, checks it, and shows the
// result before creating anything.
type MachineSpec struct {
	PluginID string
	// Name is the new target's: a slug, as a workspace's (machineName).
	Name string
	// Size, Persistent and Egress are the plugin's defaults when empty
	// (nil); the default egress is never trusted.
	Size       string
	Persistent *bool
	Egress     string
}

// MachinePlan is a MachineSpec resolved against the plugin's offer as it
// stands: exactly what a confirmation creates, and what the offer shows.
type MachinePlan struct {
	PluginID   string
	Plugin     string
	Label      string
	Name       string
	Size       MachineSize
	Persistent bool
	Egress     string
	// Machines and MaxMachines are the plugin's count and limit when
	// planned; not part of what is created.
	Machines    int
	MaxMachines int
}

// MachineMaker creates machines from plugins for the router (LOOM-216).
// app implements it over providers.Manager.
type MachineMaker interface {
	// Offers lists the plugin instances that can create a machine now.
	Offers(ctx context.Context) ([]MachineOffer, error)
	// Plan resolves spec as creating it would. A *MachineRefusal says, in
	// words for the user, why it can't be made; any other error is the
	// router's to report. Egress trusted is planned only when the plugin
	// says that name may have it.
	Plan(ctx context.Context, spec MachineSpec) (MachinePlan, error)
	// Create makes plan's target and machine, returning the target's id
	// once its row exists; the machine comes up in the background. It
	// makes exactly plan: a plugin whose offer resolves differently by
	// then is a *MachineRefusal, and nothing is made.
	Create(ctx context.Context, plan MachinePlan) (targetID string, err error)
}

// MachineRefusal is a machine that can't be made, and why, in plain
// words (LOOM-216).
type MachineRefusal struct{ Reason string }

func (e *MachineRefusal) Error() string { return e.Reason }

// WithMachineMaker lets the router propose new machines (LOOM-216):
// provision_target is offered to the routing model while m has an offer.
func WithMachineMaker(m MachineMaker) Option {
	return func(r *Router) { r.machineMaker = m }
}

// NoMachinePluginReply is the answer to a request for a new machine
// when no plugin can make one (LOOM-216's interim answer).
const NoMachinePluginReply = "Loomux can't create a machine right now: no machine plugin is installed and running. " +
	"Install one (Kubernetes or Docker) on the Plugins page or with POST /api/v1/plugins, then ask again; " +
	"or create one with POST /api/v1/targets and a plugin object, or register a machine you already have there."

// machineName is what a machine proposed in chat may be called: a
// workspace's slug, so nothing odd reaches a plugin's object names.
var machineName = regexp.MustCompile(WorkspaceNamePattern)

// machineOffers is where a machine can be created now; none without a
// MachineMaker or when they can't be listed (logged).
func (r *Router) machineOffers(ctx context.Context, log *slog.Logger) []MachineOffer {
	if r.machineMaker == nil {
		return nil
	}
	offers, err := r.machineMaker.Offers(ctx)
	if err != nil {
		log.Warn("machine offers unavailable", "error", err)
		return nil
	}
	return offers
}

// proposeMachine ends a turn whose decision is provision_target with an
// offer: the machine as it would be made, resolved against the plugin's
// offer now, waiting for a yes. Nothing is created here.
func (r *Router) proposeMachine(ctx context.Context, log *slog.Logger, conversationID, message string, d Decision, start time.Time) (string, error) {
	spec := d.NewTarget
	refuse := func(reason string) (string, error) {
		log.Info("machine not offered", "plugin", spec.PluginID, "reason", reason)
		return r.finishTurn(ctx, log, conversationID, message, "", "I can't create that machine: "+reason, "machine_refused", start)
	}
	if !machineName.MatchString(spec.Name) {
		return refuse("its name must be lowercase letters, digits, dashes and underscores, starting with a letter or digit, at most 63 characters.")
	}
	if taken, err := r.targetNameTaken(ctx, spec.Name); err != nil {
		return "", fmt.Errorf("router: dispatch: provision target: %w", err)
	} else if taken {
		return refuse(fmt.Sprintf("a target named %s already exists.", spec.Name))
	}
	plan, err := r.machineMaker.Plan(ctx, spec)
	var refusal *MachineRefusal
	if errors.As(err, &refusal) {
		return refuse(refusal.Reason)
	}
	if err != nil {
		log.Error("dispatch failed", "stage", "plan machine", "error", err)
		return "", fmt.Errorf("router: dispatch: provision target: %w", err)
	}
	// Trusted only ever as asked for, never as a default (LOOM-226).
	if plan.Egress == egressTrusted && spec.Egress != egressTrusted {
		return refuse("its network would be trusted, which is only ever made when asked for.")
	}
	if err := r.offer(ctx, conversationID, pendingInstall{kind: pendingCreateTarget, machine: &plan},
		registry.Confirmation{Kind: registry.ConfirmationCreateTarget, TargetName: plan.Name, Command: planSummary(plan)}); err != nil {
		log.Error("dispatch failed", "stage", "record offer", "error", err)
		return "", fmt.Errorf("router: dispatch: provision target: %w", err)
	}
	var b strings.Builder
	b.WriteString("I can create a new machine for this:\n\n")
	writePlan(&b, plan)
	b.WriteString("\nReply \"yes\" to create it. Any other reply cancels.")
	return r.finishTurn(ctx, log, conversationID, message, "", b.String(), "machine_offered", start)
}

// confirmMachine creates a confirmed offer's machine. The plugin's
// offer is resolved again first: if what it would make now differs from
// what was shown, nothing is created, since a yes creates only what the
// offer showed. It settles the offer's record: approved once the machine
// is being created, denied with the reason on every other way out.
func (r *Router) confirmMachine(ctx context.Context, log *slog.Logger, conversationID, message string, p pendingInstall, start time.Time) (string, error) {
	shown := *p.machine
	log.Info("machine confirmed", "plugin", shown.PluginID, "size", shown.Size.Name, "egress", shown.Egress)
	refuse := func(reason string) (string, error) {
		r.refuseOffer(ctx, log, p, reason)
		return r.finishTurn(ctx, log, conversationID, message, "", "I didn't create "+shown.Name+": "+reason, "machine_refused", start)
	}
	fail := func(stage string, err error) (string, error) {
		r.refuseOffer(ctx, log, p, "creating it failed in Loomux; the server log says why.")
		log.Error("dispatch failed", "stage", stage, "error", err)
		return "", fmt.Errorf("router: dispatch: provision target: %w", err)
	}
	if taken, err := r.targetNameTaken(ctx, shown.Name); err != nil {
		return fail("check machine name", err)
	} else if taken {
		return refuse(fmt.Sprintf("a target named %s was registered meanwhile.", shown.Name))
	}
	persistent := shown.Persistent
	plan, err := r.machineMaker.Plan(ctx, MachineSpec{PluginID: shown.PluginID, Name: shown.Name, Size: shown.Size.Name,
		Persistent: &persistent, Egress: shown.Egress})
	var refusal *MachineRefusal
	if errors.As(err, &refusal) {
		return refuse(refusal.Reason)
	}
	if err != nil {
		return fail("plan machine", err)
	}
	if !samePlan(plan, shown) {
		return refuse("the plugin's offer changed since I asked, so it would not be the machine I showed you. " +
			"Ask again to see what it offers now.")
	}
	targetID, err := r.machineMaker.Create(ctx, plan)
	if errors.As(err, &refusal) {
		return refuse(refusal.Reason)
	}
	if err != nil {
		return fail("create machine", err)
	}
	r.resolveOffer(ctx, log, p, registry.ConfirmationApproved)
	r.recordEvent(ctx, conversationID, registry.DispatchEvent{Kind: registry.EventMachine, TargetID: targetID,
		Outcome: "machine creating", Detail: "machine " + plan.Name + ": " + planSummary(plan)})
	log.Info("machine created", "target_id", targetID, "plugin", plan.PluginID)
	reply := fmt.Sprintf("Creating %s with the %s plugin. It is coming up now, which takes a minute or two "+
		"(longer while its image is pulled the first time). Once it is ready, ask for work on it.", plan.Name, pluginTitle(plan.Plugin, plan.Label))
	return r.finishTurn(ctx, log, conversationID, message, "", reply, "machine_created", start)
}

// samePlan reports whether a and b would make the same machine; the
// plugin's counts are not part of it.
func samePlan(a, b MachinePlan) bool {
	a.Machines, a.MaxMachines, b.Machines, b.MaxMachines = 0, 0, 0, 0
	return a == b
}

// targetNameTaken reports whether a target already has name.
func (r *Router) targetNameTaken(ctx context.Context, name string) (bool, error) {
	targets, err := r.store.ListTargets(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range targets {
		if t.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// egressTrusted is protocol.EgressTrusted, which the router doesn't
// import.
const egressTrusted = "trusted"

// writePlan lays plan out for the user, one line per choice.
func writePlan(b *strings.Builder, p MachinePlan) {
	fmt.Fprintf(b, "    name:     %s\n", p.Name)
	fmt.Fprintf(b, "    plugin:   %s\n", pluginTitle(p.Plugin, p.Label))
	fmt.Fprintf(b, "    size:     %s\n", sizeText(p.Size))
	if p.Persistent {
		b.WriteString("    data:     kept: its files survive stop, start and recreate\n")
	} else {
		b.WriteString("    data:     ephemeral: its files go when it is destroyed\n")
	}
	fmt.Fprintf(b, "    network:  %s\n", egressText(p.Egress))
	if p.MaxMachines > 0 {
		fmt.Fprintf(b, "    machines: %d of at most %d with this plugin, before this one\n", p.Machines, p.MaxMachines)
	}
}

// planSummary is plan on one line, for the offer's card and the audit
// trail.
func planSummary(p MachinePlan) string {
	data := "keeps its data"
	if !p.Persistent {
		data = "ephemeral"
	}
	return fmt.Sprintf("plugin %s; size %s; %s; egress %s", pluginTitle(p.Plugin, p.Label), sizeText(p.Size), data, p.Egress)
}

func pluginTitle(plugin, label string) string {
	if label == "" || label == plugin {
		return plugin
	}
	return plugin + " (" + label + ")"
}

// sizeText is "medium: 2 CPU, 4Gi memory, 20Gi disk", with what a
// Burstable size requests.
func sizeText(s MachineSize) string {
	text := fmt.Sprintf("%s: %s CPU, %s memory, %s disk", s.Name, s.CPU, s.Memory, s.Disk)
	var req []string
	if s.CPURequest != "" {
		req = append(req, s.CPURequest+" CPU")
	}
	if s.MemoryRequest != "" {
		req = append(req, s.MemoryRequest+" memory")
	}
	if len(req) > 0 {
		text += " (placed by its request of " + strings.Join(req, " and ") + ")"
	}
	return text
}

func egressText(e string) string {
	switch e {
	case "none":
		return "none: DNS only, nothing else"
	case egressTrusted:
		return "trusted: the LAN and the tailnet as well as the internet"
	}
	return e
}
