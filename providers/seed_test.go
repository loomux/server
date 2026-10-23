package providers_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/registry"
)

// LOOM-236: LOOMUX_PLUGINS_SEED's "machines".

func machineSeed(t *testing.T, raw string) *plugins.Seed {
	t.Helper()
	s, err := plugins.ParseSeed(raw)
	if err != nil {
		t.Fatalf("ParseSeed: %v", err)
	}
	return s
}

func applyMachines(t *testing.T, h *harness, s *plugins.Seed, dryRun bool) map[string]plugins.SeedAction {
	t.Helper()
	actions, pending := h.m.ApplyMachineSeed(context.Background(), s, dryRun)
	if pending {
		t.Fatalf("pending: %+v", actions)
	}
	out := map[string]plugins.SeedAction{}
	for _, a := range actions {
		out[a.Label] = a
	}
	return out
}

func targetNamed(t *testing.T, h *harness, name string) *registry.Target {
	t.Helper()
	list, err := h.store.ListTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range list {
		if tg.Name == name {
			return tg
		}
	}
	return nil
}

func seedOwnedIDs(t *testing.T, h *harness) []string {
	t.Helper()
	raw, err := h.store.GetSetting(context.Background(), registry.SettingMachinesSeedOwned)
	if err != nil {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

const ccSeed = `{"machines": [{"name": "cc", "plugin": "fake", "size": "small", "persistent": true,
	"egress": "internet", "require_confirmation": true, "relay": "last_message", "permission_mode": "accept_edits",
	"keep_idle_sessions": true}]}`

func TestSeedMachineCreatedThenNothingToDo(t *testing.T) {
	h := newHarness(t, nil)
	s := machineSeed(t, ccSeed)

	got := applyMachines(t, h, s, false)["cc"]
	if got.Do != plugins.SeedCreate || got.Warning != "" {
		t.Fatalf("first apply = %+v, want create", got)
	}
	cc := targetNamed(t, h, "cc")
	if cc == nil {
		t.Fatal("no target cc")
	}
	if !cc.Policy.RequireConfirmation || cc.Policy.Relay != registry.RelayLastMessage || cc.PermissionMode != registry.PermissionModeAcceptEdits ||
		!cc.Policy.KeepIdleSessions {
		t.Errorf("target options not carried: mode %q policy %+v", cc.PermissionMode, cc.Policy)
	}
	env := h.waitStatus(t, cc.ID, registry.EnvironmentRunning)
	if env.Size != "small" || !env.Persistent || env.Egress != "internet" || env.PluginID != h.pluginID {
		t.Errorf("machine = %+v", env)
	}
	if ids := seedOwnedIDs(t, h); !slices.Equal(ids, []string{cc.ID}) {
		t.Errorf("owned = %v, want [%s]", ids, cc.ID)
	}

	again := applyMachines(t, h, s, false)["cc"]
	if again.Do != plugins.SeedUnchanged || again.Warning != "" {
		t.Errorf("second apply = %+v, want unchanged", again)
	}
	if list, _ := h.store.ListTargets(context.Background()); len(list) != 1 {
		t.Errorf("a re-run made more targets: %d", len(list))
	}
	if after, _ := h.m.Get(context.Background(), cc.ID); after.ID != env.ID || after.Status != registry.EnvironmentRunning {
		t.Errorf("a re-run touched the machine: %+v", after)
	}
}

func TestSeedMachineSafeUpdate(t *testing.T) {
	h := newHarness(t, nil)
	applyMachines(t, h, machineSeed(t, ccSeed), false)
	cc := targetNamed(t, h, "cc")
	env := h.waitStatus(t, cc.ID, registry.EnvironmentRunning)

	changed := machineSeed(t, `{"machines": [{"name": "cc", "plugin": "fake", "size": "small", "persistent": true,
		"require_confirmation": false, "relay": "none", "permission_mode": "manual", "allowed_agent_types": ["claude-code"],
		"allow_shell": false, "keep_idle_sessions": false}]}`)
	got := applyMachines(t, h, changed, false)["cc"]
	if got.Do != plugins.SeedUpdate || got.Warning != "" {
		t.Fatalf("apply = %+v, want update", got)
	}
	for _, f := range []string{"permission_mode", "allowed_agent_types", "allow_shell", "require_confirmation", "relay", "keep_idle_sessions"} {
		if !strings.Contains(got.Detail, f) {
			t.Errorf("detail %q doesn't name %s", got.Detail, f)
		}
	}
	after := targetNamed(t, h, "cc")
	if after.PermissionMode != registry.PermissionModeManual || after.Policy.Relay != registry.RelayNone ||
		after.Policy.RequireConfirmation || !after.Policy.NoShell || !slices.Equal(after.Policy.AllowedAgentTypes, []string{"claude-code"}) ||
		after.Policy.KeepIdleSessions {
		t.Errorf("target after update: mode %q policy %+v", after.PermissionMode, after.Policy)
	}
	if after.Host != cc.Host || after.SSHKeyRef != cc.SSHKeyRef || after.HostKeys != cc.HostKeys {
		t.Error("an options update changed the machine's connection")
	}
	if e, _ := h.m.Get(context.Background(), cc.ID); e.ID != env.ID || e.Status != registry.EnvironmentRunning {
		t.Errorf("an options update touched the machine: %+v", e)
	}
	// Leaving a field out says nothing: the update stands.
	quiet := machineSeed(t, `{"machines": [{"name": "cc", "plugin": "fake"}]}`)
	if got := applyMachines(t, h, quiet, false)["cc"]; got.Do != plugins.SeedUnchanged {
		t.Errorf("an entry without options = %+v, want unchanged", got)
	}
}

func TestSeedMachineUnsafeChangeIsOnlyLogged(t *testing.T) {
	h := newHarness(t, nil)
	applyMachines(t, h, machineSeed(t, ccSeed), false)
	cc := targetNamed(t, h, "cc")
	env := h.waitStatus(t, cc.ID, registry.EnvironmentRunning)

	unsafe := machineSeed(t, `{"machines": [{"name": "cc", "plugin": "fake", "size": "medium", "persistent": false,
		"require_confirmation": true, "relay": "last_message", "permission_mode": "accept_edits"}]}`)
	plan := applyMachines(t, h, unsafe, true)["cc"]
	got := applyMachines(t, h, unsafe, false)["cc"]
	for _, a := range []plugins.SeedAction{plan, got} {
		if a.Do != plugins.SeedUnchanged {
			t.Errorf("do = %q, want unchanged (only the options are applied)", a.Do)
		}
		if !strings.Contains(a.Warning, "size small → medium needs a recreate: POST /api/v1/targets/"+cc.ID+"/recreate") ||
			!strings.Contains(a.Warning, "persistent true → false needs a new machine") {
			t.Errorf("warning = %q", a.Warning)
		}
	}
	e, _ := h.m.Get(context.Background(), cc.ID)
	if e.ID != env.ID || e.Size != "small" || !e.Persistent || e.Status != registry.EnvironmentRunning {
		t.Errorf("the machine was changed: %+v", e)
	}
	if !strings.Contains(plugins.SeedSummary([]plugins.SeedAction{got}), "[not done: size small → medium") {
		t.Errorf("summary hides the warning: %s", plugins.SeedSummary([]plugins.SeedAction{got}))
	}
}

func TestSeedMachinePruneNeverDestroys(t *testing.T) {
	h := newHarness(t, nil)
	applyMachines(t, h, machineSeed(t, ccSeed), false)
	cc := targetNamed(t, h, "cc")
	h.waitStatus(t, cc.ID, registry.EnvironmentRunning)

	for _, raw := range []string{`{"prune": true, "machines": []}`, `{"prune": false}`} {
		got := applyMachines(t, h, machineSeed(t, raw), false)["cc"]
		if got.Do != plugins.SeedKeep || !strings.Contains(got.Detail, "never destroys") {
			t.Errorf("%s: = %+v, want keep", raw, got)
		}
		if e, err := h.m.Get(context.Background(), cc.ID); err != nil || e.Status != registry.EnvironmentRunning {
			t.Fatalf("%s: the machine is gone or changed: %+v %v", raw, e, err)
		}
		if ids := seedOwnedIDs(t, h); !slices.Equal(ids, []string{cc.ID}) {
			t.Errorf("%s: owned = %v: an unlisted machine stays the seed's", raw, ids)
		}
	}
	// Listed again, it is the seed's again: nothing is made twice.
	if got := applyMachines(t, h, machineSeed(t, ccSeed), false)["cc"]; got.Do != plugins.SeedUnchanged {
		t.Errorf("listed again = %+v", got)
	}
}

func TestSeedMachineDryRunPlans(t *testing.T) {
	h := newHarness(t, nil)
	s := machineSeed(t, `{"plugins": [{"label": "later", "plugin": "fake", "source": "bundled"}],
		"machines": [{"name": "cc", "plugin": "fake"}, {"name": "next", "plugin": "later"}]}`)
	got := applyMachines(t, h, s, true)
	if got["cc"].Do != plugins.SeedCreate {
		t.Errorf("cc = %+v, want create", got["cc"])
	}
	if got["next"].Do != plugins.SeedCreate || !strings.Contains(got["next"].Detail, "after its plugin instance later") {
		t.Errorf("next = %+v, want create after its instance", got["next"])
	}
	if list, _ := h.store.ListTargets(context.Background()); len(list) != 0 {
		t.Errorf("a dry run made targets: %d", len(list))
	}
	if _, err := h.store.GetSetting(context.Background(), registry.SettingMachinesSeedOwned); err == nil {
		t.Error("a dry run recorded owned machines")
	}
	if h.m.RunMachineSeed(context.Background(), s, true) {
		t.Error("RunMachineSeed dry run reports pending")
	}
	if list, _ := h.store.ListTargets(context.Background()); len(list) != 0 {
		t.Errorf("RunMachineSeed's dry run made targets: %d", len(list))
	}
}

func TestSeedMachineLeavesOthersAlone(t *testing.T) {
	h := newHarness(t, nil)
	manual, _ := h.create(t, "manual", true)
	s := machineSeed(t, `{"machines": [{"name": "manual", "plugin": "fake", "relay": "none", "size": "medium"}]}`)
	got := applyMachines(t, h, s, false)["manual"]
	if got.Do != plugins.SeedNotOurs {
		t.Errorf("= %+v, want not the seed's", got)
	}
	if after := targetNamed(t, h, "manual"); after.Policy.Relay != manual.Policy.Relay {
		t.Error("the seed changed a machine it didn't make")
	}
	if ids := seedOwnedIDs(t, h); len(ids) != 0 {
		t.Errorf("owned = %v", ids)
	}
}

func TestSeedMachineWithoutItsInstanceIsPending(t *testing.T) {
	h := newHarness(t, nil)
	s := machineSeed(t, `{"machines": [{"name": "cc", "plugin": "missing"}]}`)
	actions, pending := h.m.ApplyMachineSeed(context.Background(), s, false)
	if !pending || len(actions) != 1 || actions[0].Do != plugins.SeedError || !strings.Contains(actions[0].Detail, `"missing"`) {
		t.Errorf("= %+v pending %v, want a pending error", actions, pending)
	}
	if list, _ := h.store.ListTargets(context.Background()); len(list) != 0 {
		t.Error("a target was made without its instance")
	}
}

func TestSeedMachineDeletedThroughTheAPIIsMadeAgain(t *testing.T) {
	h := newHarness(t, nil)
	s := machineSeed(t, ccSeed)
	applyMachines(t, h, s, false)
	first := targetNamed(t, h, "cc")
	h.waitStatus(t, first.ID, registry.EnvironmentRunning)
	if err := h.m.DeleteTarget(context.Background(), first.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	h.m.WaitJobsForTest()
	if targetNamed(t, h, "cc") != nil {
		t.Fatal("DeleteTarget left the target")
	}
	got := applyMachines(t, h, s, false)["cc"]
	second := targetNamed(t, h, "cc")
	if got.Do != plugins.SeedCreate || second == nil || second.ID == first.ID {
		t.Fatalf("= %+v, target %+v: want a new machine", got, second)
	}
	if ids := seedOwnedIDs(t, h); !slices.Equal(ids, []string{second.ID}) {
		t.Errorf("owned = %v, want only the new one", ids)
	}
}
