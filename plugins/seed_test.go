package plugins_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/registry"
)

// LOOM-219: LOOMUX_PLUGINS_SEED.

func parseSeed(t *testing.T, raw string) *plugins.Seed {
	t.Helper()
	s, err := plugins.ParseSeed(raw)
	if err != nil {
		t.Fatalf("ParseSeed: %v", err)
	}
	return s
}

// apply runs the seed for real and returns label → action.
func apply(t *testing.T, m *plugins.Manager, s *plugins.Seed) map[string]plugins.SeedAction {
	t.Helper()
	actions, pending := m.ApplySeed(context.Background(), s, false)
	if pending {
		t.Fatalf("pending after apply: %+v", actions)
	}
	return byLabel(actions)
}

func byLabel(actions []plugins.SeedAction) map[string]plugins.SeedAction {
	out := map[string]plugins.SeedAction{}
	for _, a := range actions {
		out[a.Label] = a
	}
	return out
}

// stored is the instance labelled label, its secrets included.
func stored(t *testing.T, m *plugins.Manager, store registry.Store, label string) *registry.Plugin {
	t.Helper()
	views, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Label == label {
			p, err := store.GetPlugin(context.Background(), v.ID)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	return nil
}

const seedOne = `{"plugins": [{"label": "seeded", "plugin": "fake", "source": "bundled",
	"config": {"greeting": "yo"}, "secrets": {"token": {"env": "LOOMUX_TEST_SEED_TOKEN"}}}]}`

func TestSeedCreatesThenDoesNothing(t *testing.T) {
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "s3cret")
	m, store, _ := newManager(t)
	s := parseSeed(t, seedOne)

	if got := apply(t, m, s)["seeded"]; got.Do != plugins.SeedCreate || got.Detail != "" {
		t.Fatalf("first apply = %+v, want create", got)
	}
	p := stored(t, m, store, "seeded")
	if p == nil || p.Config["greeting"] != "yo" || p.Secrets["token"] != "s3cret" || p.Status != registry.PluginStatusInstalled {
		t.Fatalf("installed = %+v", p)
	}
	before := p.UpdatedAt
	if got := apply(t, m, s)["seeded"]; got.Do != plugins.SeedUnchanged {
		t.Errorf("second apply = %+v, want unchanged", got)
	}
	if p := stored(t, m, store, "seeded"); !p.UpdatedAt.Equal(before) {
		t.Error("an unchanged seed wrote the instance")
	}
	views, _ := m.List(context.Background())
	if len(views) != 1 {
		t.Errorf("instances = %d, want 1", len(views))
	}
}

func TestSeedUpdatesWhatItMade(t *testing.T) {
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "s3cret")
	m, store, _ := newManager(t)
	apply(t, m, parseSeed(t, seedOne))

	changed := strings.Replace(seedOne, `"yo"`, `"hey"`, 1)
	if got := apply(t, m, parseSeed(t, changed))["seeded"]; got.Do != plugins.SeedUpdate {
		t.Fatalf("changed config = %+v, want update", got)
	}
	if p := stored(t, m, store, "seeded"); p.Config["greeting"] != "hey" || p.Secrets["token"] != "s3cret" {
		t.Errorf("after the update = %+v / %v", p.Config, p.Secrets)
	}
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "rotated")
	if got := apply(t, m, parseSeed(t, changed))["seeded"]; got.Do != plugins.SeedUpdate {
		t.Fatalf("rotated secret = %+v, want update", got)
	}
	if p := stored(t, m, store, "seeded"); p.Secrets["token"] != "rotated" {
		t.Errorf("token = %q, want the rotated one", p.Secrets["token"])
	}
	noSecret := `{"plugins": [{"label": "seeded", "plugin": "fake", "source": "bundled", "config": {"greeting": "hey"}}]}`
	apply(t, m, parseSeed(t, noSecret))
	if p := stored(t, m, store, "seeded"); p.Secrets["token"] != "" {
		t.Errorf("a secret dropped from the seed is still set: %q", p.Secrets["token"])
	}
}

// An instance made through the API is never the seed's, even with a
// label the seed lists.
func TestSeedLeavesOthersAlone(t *testing.T) {
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "s3cret")
	m, store, _ := newManager(t)
	install(t, m, "seeded", map[string]any{"greeting": "by hand"})
	if got := apply(t, m, parseSeed(t, seedOne))["seeded"]; got.Do != plugins.SeedNotOurs {
		t.Fatalf("apply over an instance made by hand = %+v", got)
	}
	if p := stored(t, m, store, "seeded"); p.Config["greeting"] != "by hand" {
		t.Errorf("the seed changed an instance it didn't make: %v", p.Config)
	}
	// Nor is it pruned.
	prune := parseSeed(t, `{"prune": true, "plugins": []}`)
	apply(t, m, prune)
	if stored(t, m, store, "seeded") == nil {
		t.Error("prune removed an instance the seed didn't make")
	}
}

func TestSeedPrunesOnlyWhenAsked(t *testing.T) {
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "s3cret")
	m, store, _ := newManager(t)
	apply(t, m, parseSeed(t, seedOne))

	empty := parseSeed(t, `{"plugins": []}`)
	if got := apply(t, m, empty)["seeded"]; got.Do != plugins.SeedKeep {
		t.Fatalf("an entry removed, prune off = %+v, want keep", got)
	}
	if stored(t, m, store, "seeded") == nil {
		t.Fatal("removed without prune")
	}

	machines := 1
	m.MachineCounter = func(context.Context, string) (int, error) { return machines, nil }
	prune := parseSeed(t, `{"prune": true, "plugins": []}`)
	if got := apply(t, m, prune)["seeded"]; got.Do != plugins.SeedKeep || !strings.Contains(got.Detail, "owns 1 machine") {
		t.Fatalf("prune with a machine = %+v, want keep", got)
	}
	machines = 0
	if got := apply(t, m, prune)["seeded"]; got.Do != plugins.SeedPrune {
		t.Fatalf("prune = %+v", got)
	}
	if stored(t, m, store, "seeded") != nil {
		t.Error("prune left the instance")
	}
	if got := apply(t, m, prune); len(got) != 0 {
		t.Errorf("after the prune = %+v, want nothing", got)
	}
}

func TestSeedDryRunChangesNothing(t *testing.T) {
	t.Setenv("LOOMUX_TEST_SEED_TOKEN", "s3cret")
	m, store, _ := newManager(t)
	actions, _ := m.ApplySeed(context.Background(), parseSeed(t, seedOne), true)
	if got := byLabel(actions)["seeded"]; got.Do != plugins.SeedCreate {
		t.Fatalf("dry run = %+v, want create", got)
	}
	if stored(t, m, store, "seeded") != nil {
		t.Error("a dry run installed")
	}
	if !m.RunSeed(context.Background(), parseSeed(t, seedOne), true) && stored(t, m, store, "seeded") != nil {
		t.Error("RunSeed's dry run installed")
	}
}

// Problems only the catalog or the environment reveal fail their entry,
// clearly, and leave the rest of the seed to apply.
func TestSeedEntryErrors(t *testing.T) {
	m, store, _ := newManager(t)
	s := parseSeed(t, `{"plugins": [
		{"label": "inline", "plugin": "fake", "source": "bundled", "config": {"token": "s3cret"}},
		{"label": "noenv", "plugin": "fake", "source": "bundled", "secrets": {"token": {"env": "LOOMUX_TEST_SEED_UNSET"}}},
		{"label": "missing", "plugin": "nosuch", "source": "bundled"},
		{"label": "fine", "plugin": "fake", "source": "bundled"}]}`)
	actions, pending := m.ApplySeed(context.Background(), s, false)
	got := byLabel(actions)
	if a := got["inline"]; a.Do != plugins.SeedError || !strings.Contains(a.Detail, `"token" is a secret`) {
		t.Errorf("an inline secret = %+v", a)
	}
	if a := got["noenv"]; a.Do != plugins.SeedError || !strings.Contains(a.Detail, "LOOMUX_TEST_SEED_UNSET isn't set") {
		t.Errorf("an unset env reference = %+v", a)
	}
	if a := got["missing"]; a.Do != plugins.SeedError || !pending {
		t.Errorf("a plugin not in the catalog = %+v (pending %v), want an error to retry", a, pending)
	}
	if a := got["fine"]; a.Do != plugins.SeedCreate {
		t.Errorf("the good entry = %+v", a)
	}
	for _, label := range []string{"inline", "noenv", "missing"} {
		if stored(t, m, store, label) != nil {
			t.Errorf("%s was installed", label)
		}
	}
}

func TestParseSeed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(file, []byte(seedOne), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := plugins.ParseSeed(file); err != nil || len(s.Plugins) != 1 || s.Plugins[0].Secrets["token"].Env != "LOOMUX_TEST_SEED_TOKEN" {
		t.Errorf("from a file = %+v, %v", s, err)
	}
	if s, err := plugins.ParseSeed("  "); s != nil || err != nil {
		t.Errorf("unset = %+v, %v", s, err)
	}
	for name, raw := range map[string]string{
		"not JSON":        `{"plugins": [`,
		"unknown field":   `{"plugins": [], "prnue": true}`,
		"no such file":    filepath.Join(t.TempDir(), "absent.json"),
		"bad label":       `{"plugins": [{"label": "Bad Label", "plugin": "fake", "source": "bundled"}]}`,
		"label twice":     `{"plugins": [{"label": "a", "plugin": "fake", "source": "bundled"}, {"label": "a", "plugin": "fake", "source": "bundled"}]}`,
		"no plugin":       `{"plugins": [{"label": "a", "source": "bundled"}]}`,
		"bad source":      `{"plugins": [{"label": "a", "plugin": "fake", "source": "web"}]}`,
		"both refs":       `{"plugins": [{"label": "a", "plugin": "fake", "source": "bundled", "secrets": {"token": {"env": "X", "file": "/y"}}}]}`,
		"no ref":          `{"plugins": [{"label": "a", "plugin": "fake", "source": "bundled", "secrets": {"token": {}}}]}`,
		"inline and ref":  `{"plugins": [{"label": "a", "plugin": "fake", "source": "bundled", "config": {"token": "x"}, "secrets": {"token": {"env": "X"}}}]}`,
		"two JSON values": `{"plugins": []} {}`,
	} {
		if _, err := plugins.ParseSeed(raw); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// A secret read from a file loses its trailing newline, as a mounted
// Kubernetes Secret has one.
func TestSeedSecretFromFile(t *testing.T) {
	m, store, _ := newManager(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	apply(t, m, parseSeed(t, `{"plugins": [{"label": "f", "plugin": "fake", "source": "bundled", "secrets": {"token": {"file": "`+file+`"}}}]}`))
	if p := stored(t, m, store, "f"); p == nil || p.Secrets["token"] != "from-a-file" {
		t.Errorf("token = %+v", p)
	}
}
