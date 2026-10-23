package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/Loomux/server/registry"
)

// The plugin seed (LOOM-219, design §13.19): plugin instances declared
// by the deployment, for GitOps, in LOOMUX_PLUGINS_SEED as JSON or as
// the path of a JSON file:
//
//	{"prune": false, "plugins": [{"label": "wysekube", "plugin": "kubernetes", "source": "socket",
//	  "config": {"namespace": "loomux-agents"},
//	  "secrets": {"kubeconfig": {"file": "/run/secrets/kubeconfig"}}}]}
//
// Applied at startup, after the installed plugins have started: an
// instance the seed lists and nothing has is installed; one the seed
// made is configured as the seed says when that differs (so a changed
// seed is a reconfigure, and an unchanged one does nothing); one made
// outside the seed (through the UI or API) is left alone, even with a
// label the seed lists. Which instances the seed made is the setting
// registry.SettingPluginsSeedOwned (their ids). An instance the seed
// made and no longer lists is kept, unless the seed says "prune": then
// it is uninstalled, if it owns no machines (destroying machines is
// never a seed's to do: remove such an instance through the API with
// targets=destroy or keep).
//
// Secrets are references, never values: a field the plugin's schema
// marks secret is refused in "config" and read from the environment
// variable or file "secrets" names, when the seed is applied.
//
// "machines" (LOOM-236) declares machines of those instances, by target
// name; providers.(*Manager).ApplyMachineSeed applies them after the
// instances (providers/seed.go). Parsing and checking them is here, so a
// bad machine entry stops startup like any other part of the seed.

// Seed is LOOMUX_PLUGINS_SEED.
type Seed struct {
	// Prune uninstalls the instances the seed made and no longer lists
	// (those without machines).
	Prune   bool        `json:"prune"`
	Plugins []SeedEntry `json:"plugins"`
	// Machines are persistent machines to make on those instances
	// (LOOM-236): made when missing, their policy kept as the seed says,
	// never destroyed (prune doesn't apply to them).
	Machines []SeedMachine `json:"machines"`
}

// SeedMachine is one machine the seed declares: a target a plugin
// instance makes, as POST /api/v1/targets with a plugin object would.
// Name is the target's name, and is what the seed matches it by. A
// pointer field left out says nothing: the API's default on a create,
// the machine's current value after that.
type SeedMachine struct {
	Name string `json:"name"`
	// Plugin is the label of the plugin instance that makes it (seeded
	// or not).
	Plugin string `json:"plugin"`
	// Size, Persistent and Egress shape the machine; empty or left out
	// is the plugin's default. On an existing machine they are only
	// compared: changing one needs a recreate or a new machine, which the
	// seed logs and leaves to the API.
	Size       string `json:"size"`
	Persistent *bool  `json:"persistent"`
	Egress     string `json:"egress"`
	// The target options a machine takes (POST and PUT /api/v1/targets);
	// these can change on an existing machine, and the seed keeps them.
	PermissionMode      *string   `json:"permission_mode"`
	Purpose             *string   `json:"purpose"`
	AllowedAgentTypes   *[]string `json:"allowed_agent_types"`
	AllowProvision      *bool     `json:"allow_provision"`
	AllowShell          *bool     `json:"allow_shell"`
	RequireConfirmation *bool     `json:"require_confirmation"`
	Relay               *string   `json:"relay"`
}

// ApplyTargetOptions sets on t the target options m gives, spelled the
// way the store keeps them (as the API's target decoding does).
func (m SeedMachine) ApplyTargetOptions(t *registry.Target) {
	if m.PermissionMode != nil {
		t.PermissionMode = strings.ReplaceAll(*m.PermissionMode, "_", "-")
	}
	if m.Purpose != nil {
		t.Policy.Purpose = *m.Purpose
	}
	if m.AllowedAgentTypes != nil {
		t.Policy.AllowedAgentTypes = slices.Clone(*m.AllowedAgentTypes)
		if len(t.Policy.AllowedAgentTypes) == 0 {
			t.Policy.AllowedAgentTypes = nil
		}
	}
	if m.AllowProvision != nil {
		t.Policy.NoProvision = !*m.AllowProvision
	}
	if m.AllowShell != nil {
		t.Policy.NoShell = !*m.AllowShell
	}
	if m.RequireConfirmation != nil {
		t.Policy.RequireConfirmation = *m.RequireConfirmation
	}
	if m.Relay != nil {
		t.Policy.Relay = *m.Relay
	}
}

// check is what ParseSeed can tell of a machine entry without the
// plugin: a name, a valid instance label, target options the store
// accepts.
func (m SeedMachine) check() error {
	if !ValidPluginLabel(m.Plugin) {
		return fmt.Errorf("plugin %q isn't a valid plugin instance label", m.Plugin)
	}
	// The plugin sets the connection; a stand-in one lets Validate check
	// the name, permission mode and policy as it will on the real target.
	t := registry.Target{Name: m.Name, Kind: registry.TargetKindRemote, Host: "machine.invalid", User: "agent"}
	m.ApplyTargetOptions(&t)
	return t.Validate()
}

// SeedEntry is one plugin instance the seed declares.
type SeedEntry struct {
	// Label names the instance, and is what the seed matches it by.
	Label string `json:"label"`
	// Plugin and Source are the manifest's name and where it comes from
	// (bundled, dir, socket), as POST /api/v1/plugins takes them.
	Plugin string                `json:"plugin"`
	Source registry.PluginSource `json:"source"`
	// Config is the configuration without its secret fields.
	Config map[string]any `json:"config"`
	// Secrets are the secret fields, each read from an environment
	// variable or a file.
	Secrets map[string]SecretRef `json:"secrets"`
}

// SecretRef says where a secret's value is: exactly one of an
// environment variable or a file (its content, trailing newlines
// trimmed).
type SecretRef struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// ParseSeed reads LOOMUX_PLUGINS_SEED: JSON when it starts with "{",
// else the path of a file holding it. Unknown fields and a seed that
// can't be right (no label, a label twice, a secret both inline and
// referenced) are errors: the server doesn't start on one.
func ParseSeed(raw string) (*Seed, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	data := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		var err error
		if data, err = os.ReadFile(raw); err != nil {
			return nil, fmt.Errorf("plugins seed: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Seed
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("plugins seed: %w", err)
	}
	if dec.More() {
		return nil, errors.New("plugins seed: more than one JSON value")
	}
	labels := map[string]bool{}
	for i, e := range s.Plugins {
		where := fmt.Sprintf("plugins seed: entry %d", i)
		switch {
		case !ValidPluginLabel(e.Label):
			return nil, fmt.Errorf("%s: label %q isn't a valid plugin label", where, e.Label)
		case labels[e.Label]:
			return nil, fmt.Errorf("%s: label %q is listed twice", where, e.Label)
		case e.Plugin == "":
			return nil, fmt.Errorf("%s (%s): no plugin", where, e.Label)
		case e.Source != registry.PluginSourceBundled && e.Source != registry.PluginSourceDir && e.Source != registry.PluginSourceSocket:
			return nil, fmt.Errorf("%s (%s): source must be bundled, dir or socket, not %q", where, e.Label, e.Source)
		}
		labels[e.Label] = true
		for name, ref := range e.Secrets {
			if (ref.Env == "") == (ref.File == "") {
				return nil, fmt.Errorf("%s (%s): secret %q needs exactly one of env or file", where, e.Label, name)
			}
			if _, inline := e.Config[name]; inline {
				return nil, fmt.Errorf("%s (%s): %q is both in config and in secrets", where, e.Label, name)
			}
		}
	}
	names := map[string]bool{}
	for i := range s.Machines {
		mc := &s.Machines[i]
		mc.Name = strings.TrimSpace(mc.Name)
		where := fmt.Sprintf("plugins seed: machine %d", i)
		switch {
		case mc.Name == "":
			return nil, fmt.Errorf("%s: no name", where)
		case names[mc.Name]:
			return nil, fmt.Errorf("%s: name %q is listed twice", where, mc.Name)
		}
		names[mc.Name] = true
		if err := mc.check(); err != nil {
			return nil, fmt.Errorf("%s (%s): %w", where, mc.Name, err)
		}
	}
	return &s, nil
}

// What ApplySeed does with an instance (SeedAction.Do).
const (
	SeedCreate    = "create"
	SeedUpdate    = "update"
	SeedUnchanged = "unchanged"
	SeedNotOurs   = "not the seed's"
	SeedKeep      = "keep"
	SeedPrune     = "prune"
	SeedError     = "error"
)

// SeedAction is what ApplySeed did, or would do, with one instance (or
// ApplyMachineSeed with one machine, Label then its name).
type SeedAction struct {
	Label  string
	Do     string
	Detail string
	// Warning is what the seed asks for and didn't do because only the
	// API may (a machine change that needs a recreate): logged as a
	// warning beside the action.
	Warning string
}

// ApplySeed brings the installed plugins in line with s, as its doc
// says, and returns what it did; with dryRun it only works out and
// returns what it would do. pending reports entries that failed before
// anything was made (their plugin isn't in the catalog yet: a sidecar
// still starting), which a later ApplySeed may complete.
func (m *Manager) ApplySeed(ctx context.Context, s *Seed, dryRun bool) (actions []SeedAction, pending bool) {
	owned, err := m.seedOwned(ctx)
	if err != nil {
		return []SeedAction{{Do: SeedError, Detail: err.Error()}}, true
	}
	rows, err := m.store.ListPlugins(ctx)
	if err != nil {
		return []SeedAction{{Do: SeedError, Detail: err.Error()}}, true
	}
	byLabel := map[string]*registry.Plugin{}
	exists := map[string]bool{}
	for _, p := range rows {
		byLabel[p.Label], exists[p.ID] = p, true
	}
	ownedBefore := slices.Sorted(maps.Keys(owned)) // not what this run installs
	listed := map[string]bool{}
	for _, e := range s.Plugins {
		listed[e.Label] = true
		a, retry := m.seedEntry(ctx, e, byLabel[e.Label], owned, dryRun)
		actions = append(actions, a)
		pending = pending || retry
	}
	for _, id := range ownedBefore {
		if !exists[id] {
			delete(owned, id) // removed through the API since
			continue
		}
		p := rowByID(rows, id)
		if listed[p.Label] {
			continue
		}
		actions = append(actions, m.seedPrune(ctx, s.Prune, p, owned, dryRun))
	}
	if !dryRun {
		if err := m.saveSeedOwned(ctx, owned); err != nil {
			actions = append(actions, SeedAction{Do: SeedError, Detail: "record the seed's instances: " + err.Error()})
		}
	}
	return actions, pending
}

// seedEntry applies one entry.
func (m *Manager) seedEntry(ctx context.Context, e SeedEntry, p *registry.Plugin, owned map[string]bool, dryRun bool) (SeedAction, bool) {
	act := SeedAction{Label: e.Label}
	fail := func(retry bool, format string, args ...any) (SeedAction, bool) {
		act.Do, act.Detail = SeedError, fmt.Sprintf(format, args...)
		return act, retry
	}
	if p != nil && !owned[p.ID] {
		act.Do, act.Detail = SeedNotOurs, "made outside the seed; left as it is"
		return act, false
	}
	secrets, err := resolveSecrets(e.Secrets)
	if err != nil {
		return fail(true, "%v", err)
	}
	if p == nil {
		a, err := m.catalog.Find(ctx, e.Plugin, e.Source)
		if err != nil {
			return fail(true, "%v", err)
		}
		schema, err := a.Manifest.Schema()
		if err != nil {
			return fail(false, "%v", err)
		}
		if err := secretsReferenced(schema, e); err != nil {
			return fail(false, "%v", err)
		}
		act.Do = SeedCreate
		if dryRun {
			return act, false
		}
		v, err := m.Install(ctx, InstallRequest{Name: e.Plugin, Source: e.Source, Label: e.Label, Config: mergedConfig(e.Config, secrets)})
		if v == nil {
			return fail(false, "install: %v", err)
		}
		owned[v.ID] = true
		if err != nil {
			act.Detail = "installed, but: " + err.Error()
		}
		return act, false
	}
	if p.Name != e.Plugin || p.Source != e.Source {
		return fail(false, "the instance is %s from %s, the seed says %s from %s: remove it, or give the entry another label", p.Name, p.Source, e.Plugin, e.Source)
	}
	full, err := m.store.GetPlugin(ctx, p.ID)
	if err != nil {
		return fail(false, "%v", err)
	}
	manifest, err := m.installedManifest(full)
	if err != nil {
		return fail(false, "%v", err)
	}
	schema, err := manifest.Schema()
	if err != nil {
		return fail(false, "%v", err)
	}
	if err := secretsReferenced(schema, e); err != nil {
		return fail(false, "%v", err)
	}
	want := schema.ApplyDefaults(mergedConfig(e.Config, secrets))
	have := mergedConfig(full.Config, full.Secrets)
	if sameJSON(want, have) {
		act.Do = SeedUnchanged
		return act, false
	}
	act.Do = SeedUpdate
	if dryRun {
		return act, false
	}
	for _, name := range schema.SecretFields() {
		if _, set := full.Secrets[name]; set {
			if _, wanted := want[name]; !wanted {
				want[name] = "" // dropped from the seed: cleared
			}
		}
	}
	if _, err := m.SetConfig(ctx, p.ID, want); err != nil {
		act.Detail = "configured, but: " + err.Error()
	}
	return act, false
}

// seedPrune deals with an instance the seed made and no longer lists.
func (m *Manager) seedPrune(ctx context.Context, prune bool, p *registry.Plugin, owned map[string]bool, dryRun bool) SeedAction {
	act := SeedAction{Label: p.Label, Do: SeedKeep}
	if !prune {
		act.Detail = "no longer in the seed; kept, since prune is off"
		return act
	}
	n, err := m.machines(ctx, p.ID)
	switch {
	case err != nil:
		act.Do, act.Detail = SeedError, err.Error()
		return act
	case n > 0:
		act.Detail = fmt.Sprintf("no longer in the seed, but it owns %d machine(s): uninstall it through the API, choosing to destroy or keep them", n)
		return act
	}
	act.Do = SeedPrune
	if dryRun {
		return act
	}
	if err := m.Uninstall(ctx, p.ID, ""); err != nil {
		act.Do, act.Detail = SeedError, "uninstall: "+err.Error()
		return act
	}
	delete(owned, p.ID)
	return act
}

// secretsReferenced refuses a secret field given inline in config.
func secretsReferenced(schema *Schema, e SeedEntry) error {
	for _, name := range schema.SecretFields() {
		if _, inline := e.Config[name]; inline {
			return fmt.Errorf("%q is a secret: give it in secrets as {\"env\": …} or {\"file\": …}, never in config", name)
		}
	}
	return nil
}

// resolveSecrets reads each reference.
func resolveSecrets(refs map[string]SecretRef) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	for name, ref := range refs {
		if ref.Env != "" {
			v, ok := os.LookupEnv(ref.Env)
			if !ok {
				return nil, fmt.Errorf("secret %q: environment variable %s isn't set", name, ref.Env)
			}
			out[name] = v
			continue
		}
		data, err := os.ReadFile(ref.File)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %v", name, err)
		}
		out[name] = strings.TrimRight(string(data), "\r\n")
	}
	return out, nil
}

// sameJSON compares two configurations as JSON would carry them.
func sameJSON(a, b map[string]any) bool {
	norm := func(v map[string]any) any {
		data, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(data, &out)
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func (m *Manager) seedOwned(ctx context.Context) (map[string]bool, error) {
	owned := map[string]bool{}
	raw, err := m.store.GetSetting(ctx, registry.SettingPluginsSeedOwned)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return owned, nil
	case err != nil:
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("plugins seed: setting %s: %w", registry.SettingPluginsSeedOwned, err)
	}
	for _, id := range ids {
		owned[id] = true
	}
	return owned, nil
}

func (m *Manager) saveSeedOwned(ctx context.Context, owned map[string]bool) error {
	ids := slices.Sorted(maps.Keys(owned))
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return m.store.SetSetting(ctx, registry.SettingPluginsSeedOwned, string(data))
}

func rowByID(rows []*registry.Plugin, id string) *registry.Plugin {
	for _, p := range rows {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// RunSeed logs what s would do, as one "plugins seed plan" line, and
// unless dryRun applies it, logging each instance's outcome (an error as
// a warning). It reports whether entries are pending (ApplySeed).
func (m *Manager) RunSeed(ctx context.Context, s *Seed, dryRun bool) bool {
	plan, _ := m.ApplySeed(ctx, s, true)
	m.logger.Info("plugins seed plan", "dry_run", dryRun, "plan", SeedSummary(plan))
	if dryRun {
		return false
	}
	done, pending := m.ApplySeed(ctx, s, false)
	for _, a := range done {
		if a.Do == SeedError {
			m.logger.Warn("plugins seed", "label", a.Label, "do", a.Do, "error", a.Detail, "retried", pending)
			continue
		}
		m.logger.Info("plugins seed", "label", a.Label, "do", a.Do, "detail", a.Detail)
	}
	return pending
}

// SeedSummary is actions on one line: "label: do (detail) [warning]; …".
func SeedSummary(actions []SeedAction) string {
	parts := make([]string, 0, len(actions))
	for _, a := range actions {
		part := a.Label + ": " + a.Do
		if a.Label == "" {
			part = a.Do
		}
		if a.Detail != "" {
			part += " (" + a.Detail + ")"
		}
		if a.Warning != "" {
			part += " [not done: " + a.Warning + "]"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "nothing to do"
	}
	return strings.Join(parts, "; ")
}
