package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// machineOffersTTL is how long the machine offers a routing call sees
// are reused: each is a targets.describe per plugin instance, which
// reads the cluster, and every chat message asks for them.
const machineOffersTTL = 30 * time.Second

// machineMaker is router.MachineMaker over providers.Manager (LOOM-216).
type machineMaker struct {
	machines *providers.Manager
	now      func() time.Time

	mu      sync.Mutex
	offers  []router.MachineOffer
	fetched time.Time
}

func newMachineMaker(m *providers.Manager) *machineMaker {
	return &machineMaker{machines: m, now: time.Now}
}

func (m *machineMaker) Offers(ctx context.Context) ([]router.MachineOffer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.fetched.IsZero() && m.now().Sub(m.fetched) < machineOffersTTL {
		return m.offers, nil
	}
	list, err := m.machines.Offers(ctx)
	if err != nil {
		return nil, err
	}
	offers := make([]router.MachineOffer, len(list))
	for i, o := range list {
		sizes := make([]router.MachineSize, len(o.Info.Sizes))
		for j, s := range o.Info.Sizes {
			sizes[j] = machineSize(s)
		}
		offers[i] = router.MachineOffer{PluginID: o.PluginID, Plugin: o.PluginName, Label: o.PluginLabel, Sizes: sizes,
			PersistentDefault: o.Info.PersistentDefault, Persistent: o.Persistent, Ephemeral: o.Ephemeral,
			Egress: o.Info.EgressOptions, Machines: o.Info.Environments, MaxMachines: o.Info.MaxEnvironments}
	}
	m.offers, m.fetched = offers, m.now()
	return offers, nil
}

// request is spec as providers.Create takes it: a target with only its
// name (a created machine's permissive default policy, design §7), and
// egress trusted only when the plugin answers for the name.
func request(spec router.MachineSpec) providers.CreateRequest {
	return providers.CreateRequest{Target: registry.Target{Name: spec.Name}, PluginID: spec.PluginID,
		Size: spec.Size, Persistent: spec.Persistent, Egress: spec.Egress, NamedTrust: true}
}

func (m *machineMaker) Plan(ctx context.Context, spec router.MachineSpec) (router.MachinePlan, error) {
	p, err := m.machines.Plan(ctx, request(spec))
	if err != nil {
		return router.MachinePlan{}, machineRefusal(err)
	}
	return router.MachinePlan{PluginID: p.PluginID, Plugin: p.PluginName, Label: p.PluginLabel, Name: p.Name,
		Size: machineSize(p.Size), Persistent: p.Persistent, Egress: p.Egress, Machines: p.Machines, MaxMachines: p.MaxMachines}, nil
}

func (m *machineMaker) Create(ctx context.Context, plan router.MachinePlan) (string, error) {
	persistent := plan.Persistent
	target, _, err := m.machines.Create(ctx, request(router.MachineSpec{PluginID: plan.PluginID, Name: plan.Name,
		Size: plan.Size.Name, Persistent: &persistent, Egress: plan.Egress}))
	if err != nil {
		return "", machineRefusal(err)
	}
	// The next routing call sees the new count.
	m.mu.Lock()
	m.fetched = time.Time{}
	m.mu.Unlock()
	return target.ID, nil
}

func machineSize(s protocol.Size) router.MachineSize {
	return router.MachineSize{Name: s.Name, CPU: s.CPU, Memory: s.Memory, Disk: s.Disk,
		CPURequest: s.CPURequest, MemoryRequest: s.MemoryRequest}
}

// machineRefusal is err as the user is told it, when it is a machine
// that can't be made rather than Loomux failing.
func machineRefusal(err error) error {
	var invalid *providers.InvalidError
	var missing *plugins.CapabilityMissingError
	var badAddr *providers.BadAddressError
	switch {
	case errors.As(err, &invalid):
		return &router.MachineRefusal{Reason: fmt.Sprintf("%s %s.", invalid.Field, invalid.Msg)}
	case errors.Is(err, providers.ErrQuota):
		return &router.MachineRefusal{Reason: "the plugin's machine limit is reached; delete a machine it made first."}
	case errors.Is(err, providers.ErrPluginUnavailable):
		return &router.MachineRefusal{Reason: "that plugin isn't installed, enabled and running."}
	case errors.As(err, &missing):
		return &router.MachineRefusal{Reason: "that plugin doesn't make machines."}
	case errors.As(err, &badAddr):
		return &router.MachineRefusal{Reason: "the plugin's offer can't be used (" + badAddr.Detail + ")."}
	case errors.Is(err, registry.ErrConflict):
		return &router.MachineRefusal{Reason: "a target with that name exists."}
	}
	return err
}
