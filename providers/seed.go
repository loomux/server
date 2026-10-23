package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/registry"
)

// The seed's machines (LOOM-236): LOOMUX_PLUGINS_SEED's "machines", the
// persistent machines a deployment declares, applied after its plugin
// instances (plugins/seed.go). Matched by target name:
//
//   - one the seed lists and no target has that name is created, as
//     POST /api/v1/targets with a plugin object would, and recorded as
//     the seed's (registry.SettingMachinesSeedOwned, target ids);
//   - one the seed made is kept in line where that is safe on a running
//     machine: its target options (permission mode, purpose, allowed
//     agent types, provision, shell, confirmation, relay, idle sessions), the fields
//     PUT /targets/{id} may change on a machine. What needs the machine
//     made again (size: a recreate; persistence, egress, another plugin
//     instance: a new machine) is logged as a warning and left to the
//     API, since it can restart or lose the machine;
//   - a target with that name made outside the seed is left alone;
//   - one the seed made and no longer lists is kept, prune or not: a seed
//     never destroys a machine; delete it through the API.
//
// A field the entry leaves out says nothing: the plugin's or the API's
// default on a create, the machine's current value after that.

// seedAfterInstance is a dry run's create detail when the machine's
// instance is itself only in the plugin seed's plan so far.
const seedAfterInstance = "after its plugin instance"

// ApplyMachineSeed brings the machines the seed made in line with s's
// "machines", as the package comment above says, and returns what it
// did; with dryRun it only works out what it would do. pending reports
// machines not made because their plugin instance isn't there or
// running yet, which a later ApplyMachineSeed may complete.
func (m *Manager) ApplyMachineSeed(ctx context.Context, s *plugins.Seed, dryRun bool) (actions []plugins.SeedAction, pending bool) {
	owned, err := m.machineSeedOwned(ctx)
	if err != nil {
		return []plugins.SeedAction{{Do: plugins.SeedError, Detail: err.Error()}}, true
	}
	targets, err := m.store.ListTargets(ctx)
	if err != nil {
		return []plugins.SeedAction{{Do: plugins.SeedError, Detail: err.Error()}}, true
	}
	rows, err := m.store.ListPlugins(ctx)
	if err != nil {
		return []plugins.SeedAction{{Do: plugins.SeedError, Detail: err.Error()}}, true
	}
	byName := map[string]*registry.Target{}
	exists := map[string]*registry.Target{}
	for _, t := range targets {
		byName[t.Name], exists[t.ID] = t, t
	}
	instances := map[string]*registry.Plugin{}
	for _, p := range rows {
		instances[p.Label] = p
	}
	seeded := map[string]bool{} // instance labels the plugin seed lists
	for _, e := range s.Plugins {
		seeded[e.Label] = true
	}
	ownedBefore := slices.Sorted(maps.Keys(owned))
	listed := map[string]bool{}
	for _, mc := range s.Machines {
		listed[mc.Name] = true
		a, retry := m.seedMachine(ctx, mc, byName[mc.Name], instances, seeded, owned, dryRun)
		actions = append(actions, a)
		pending = pending || retry
	}
	for _, id := range ownedBefore {
		t := exists[id]
		if t == nil {
			delete(owned, id) // deleted through the API since
			continue
		}
		if listed[t.Name] {
			continue
		}
		actions = append(actions, plugins.SeedAction{Label: t.Name, Do: plugins.SeedKeep,
			Detail: "no longer in the seed; kept: a seed never destroys a machine (delete it through the API)"})
	}
	if !dryRun && (len(owned) > 0 || len(ownedBefore) > 0) {
		if err := m.saveMachineSeedOwned(ctx, owned); err != nil {
			actions = append(actions, plugins.SeedAction{Do: plugins.SeedError, Detail: "record the seed's machines: " + err.Error()})
		}
	}
	return actions, pending
}

// seedMachine applies one machine entry.
func (m *Manager) seedMachine(ctx context.Context, mc plugins.SeedMachine, t *registry.Target, instances map[string]*registry.Plugin,
	seeded, owned map[string]bool, dryRun bool) (plugins.SeedAction, bool) {
	act := plugins.SeedAction{Label: mc.Name}
	fail := func(retry bool, format string, args ...any) (plugins.SeedAction, bool) {
		act.Do, act.Detail = plugins.SeedError, fmt.Sprintf(format, args...)
		return act, retry
	}
	if t != nil && !owned[t.ID] {
		act.Do, act.Detail = plugins.SeedNotOurs, "a target with this name was made outside the seed; left as it is"
		return act, false
	}
	inst := instances[mc.Plugin]
	if t == nil {
		if inst == nil {
			if dryRun && seeded[mc.Plugin] {
				act.Do, act.Detail = plugins.SeedCreate, seedAfterInstance+" "+mc.Plugin
				return act, false
			}
			return fail(true, "no plugin instance labelled %q (yet)", mc.Plugin)
		}
		act.Do = plugins.SeedCreate
		if dryRun {
			return act, false
		}
		target := registry.Target{Name: mc.Name}
		mc.ApplyTargetOptions(&target)
		created, _, err := m.Create(ctx, CreateRequest{Target: target, PluginID: inst.ID, Size: mc.Size, Persistent: mc.Persistent, Egress: mc.Egress})
		switch {
		case errors.Is(err, ErrPluginUnavailable):
			// installed but not running yet (or disabled): try again
			return fail(true, "plugin instance %q isn't running yet", mc.Plugin)
		case err != nil:
			return fail(false, "create: %v", err)
		}
		owned[created.ID] = true
		act.Detail = "target " + created.ID
		return act, false
	}

	env, err := m.store.GetEnvironmentByTarget(ctx, t.ID)
	if errors.Is(err, registry.ErrNotFound) {
		return fail(false, "target %s is no longer a machine; the seed won't make it again under this name while it exists", t.ID)
	}
	if err != nil {
		return fail(false, "%v", err)
	}
	act.Warning = strings.Join(needsRecreate(mc, t.ID, env, inst), "; ")

	want := *t
	mc.ApplyTargetOptions(&want)
	if want.PermissionMode == t.PermissionMode && reflect.DeepEqual(want.Policy, t.Policy) {
		act.Do = plugins.SeedUnchanged
		return act, false
	}
	act.Do, act.Detail = plugins.SeedUpdate, strings.Join(changedOptions(t, &want), ", ")
	if dryRun {
		return act, false
	}
	if err := want.Validate(); err != nil {
		return fail(false, "%v", err)
	}
	if err := m.store.UpdateTarget(ctx, &want); err != nil {
		return fail(false, "update: %v", err)
	}
	return act, false
}

// needsRecreate lists what the entry asks of an existing machine that
// only making it again would do, each with how to do it through the API.
func needsRecreate(mc plugins.SeedMachine, targetID string, env *registry.Environment, inst *registry.Plugin) []string {
	var out []string
	if mc.Size != "" && mc.Size != env.Size {
		out = append(out, fmt.Sprintf("size %s → %s needs a recreate: POST /api/v1/targets/%s/recreate {\"size\": %q}", env.Size, mc.Size, targetID, mc.Size))
	}
	newMachine := "needs a new machine: delete this one through the API (its data goes with it) and the seed makes it again"
	if mc.Persistent != nil && *mc.Persistent != env.Persistent {
		out = append(out, fmt.Sprintf("persistent %t → %t %s", env.Persistent, *mc.Persistent, newMachine))
	}
	if mc.Egress != "" && mc.Egress != env.Egress {
		out = append(out, fmt.Sprintf("egress %s → %s %s", env.Egress, mc.Egress, newMachine))
	}
	switch {
	case inst == nil:
		out = append(out, fmt.Sprintf("plugin instance %q doesn't exist; the machine stays on its own", mc.Plugin))
	case env.PluginID != inst.ID:
		out = append(out, fmt.Sprintf("the machine belongs to another plugin instance than %q: %s", mc.Plugin, newMachine))
	}
	return out
}

// changedOptions names the target options want changes on t.
func changedOptions(t, want *registry.Target) []string {
	var out []string
	add := func(changed bool, name string) {
		if changed {
			out = append(out, name)
		}
	}
	add(t.PermissionMode != want.PermissionMode, "permission_mode")
	add(t.Policy.Purpose != want.Policy.Purpose, "purpose")
	add(!slices.Equal(t.Policy.AllowedAgentTypes, want.Policy.AllowedAgentTypes), "allowed_agent_types")
	add(t.Policy.NoProvision != want.Policy.NoProvision, "allow_provision")
	add(t.Policy.NoShell != want.Policy.NoShell, "allow_shell")
	add(t.Policy.RequireConfirmation != want.Policy.RequireConfirmation, "require_confirmation")
	add(t.Policy.Relay != want.Policy.Relay, "relay")
	add(t.Policy.KeepIdleSessions != want.Policy.KeepIdleSessions, "keep_idle_sessions")
	return out
}

func (m *Manager) machineSeedOwned(ctx context.Context) (map[string]bool, error) {
	owned := map[string]bool{}
	raw, err := m.store.GetSetting(ctx, registry.SettingMachinesSeedOwned)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return owned, nil
	case err != nil:
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("machines seed: setting %s: %w", registry.SettingMachinesSeedOwned, err)
	}
	for _, id := range ids {
		owned[id] = true
	}
	return owned, nil
}

func (m *Manager) saveMachineSeedOwned(ctx context.Context, owned map[string]bool) error {
	ids := slices.Sorted(maps.Keys(owned))
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return m.store.SetSetting(ctx, registry.SettingMachinesSeedOwned, string(data))
}

// RunMachineSeed logs what s's machines would do, as one "machines seed
// plan" line, and unless dryRun applies them, logging each machine's
// outcome (an error, or a change left to the API, as a warning). It
// reports whether machines are pending. A seed with no machines, and none
// made before, logs nothing.
func (m *Manager) RunMachineSeed(ctx context.Context, s *plugins.Seed, dryRun bool) bool {
	plan, _ := m.ApplyMachineSeed(ctx, s, true)
	if len(plan) == 0 {
		return false
	}
	m.logger.Info("machines seed plan", "dry_run", dryRun, "plan", plugins.SeedSummary(plan))
	if dryRun {
		return false
	}
	done, pending := m.ApplyMachineSeed(ctx, s, false)
	for _, a := range done {
		switch {
		case a.Do == plugins.SeedError:
			m.logger.Warn("machines seed", "machine", a.Label, "do", a.Do, "error", a.Detail, "retried", pending)
		case a.Warning != "":
			m.logger.Warn("machines seed", "machine", a.Label, "do", a.Do, "detail", a.Detail, "not_done", a.Warning)
		default:
			m.logger.Info("machines seed", "machine", a.Label, "do", a.Do, "detail", a.Detail)
		}
	}
	return pending
}
