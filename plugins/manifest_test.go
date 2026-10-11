package plugins

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func okManifest(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile("testdata/manifest-ok.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

func TestParseManifest(t *testing.T) {
	m := okManifest(t)
	if m.Name != "kubernetes" || m.Version != "0.4.0" || m.Protocol != ProtocolV1 {
		t.Errorf("manifest = %+v", m)
	}
	if !m.HasCapability("targets.create") || m.HasCapability("targets.recreate") {
		t.Error("HasCapability wrong")
	}
	if major, err := m.ProtocolMajor(); err != nil || major != 1 {
		t.Errorf("ProtocolMajor = %d, %v", major, err)
	}
	if !m.Equal(m) {
		t.Error("a manifest should equal itself")
	}
	other := *m
	other.Version = "0.4.1"
	if m.Equal(&other) {
		t.Error("a different version should not be equal")
	}
}

func TestManifestValidateRefuses(t *testing.T) {
	cases := map[string]func(m *Manifest){
		"bad name":             func(m *Manifest) { m.Name = "Kube_rnetes" },
		"no version":           func(m *Manifest) { m.Version = " " },
		"protocol major 2":     func(m *Manifest) { m.Protocol = "loomux-plugin/2" },
		"protocol garbage":     func(m *Manifest) { m.Protocol = "grpc" },
		"unknown capability":   func(m *Manifest) { m.Capabilities = []string{"targets.fly"} },
		"duplicate capability": func(m *Manifest) { m.Capabilities = []string{"targets.create", "targets.create"} },
		"host request":         func(m *Manifest) { m.HostRequests = []string{"vault.read"} },
		"schema not an object": func(m *Manifest) { m.ConfigSchema = json.RawMessage(`{"type":"string"}`) },
		"schema unknown type": func(m *Manifest) {
			m.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"array"}}}`)
		},
		"nested secret": func(m *Manifest) {
			m.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string","x-secret":true}}}}}`)
		},
		"non-string secret": func(m *Manifest) {
			m.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer","x-secret":true}}}`)
		},
		"required not property": func(m *Manifest) {
			m.ConfigSchema = json.RawMessage(`{"type":"object","required":["x"],"properties":{}}`)
		},
		"default off enum": func(m *Manifest) {
			m.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","enum":["x"],"default":"y"}}}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := okManifest(t)
			mutate(m)
			if err := m.Validate(); err == nil {
				t.Fatal("Validate accepted a bad manifest")
			}
		})
	}
}

func TestManifestUnknownFieldsIgnored(t *testing.T) {
	data := []byte(`{"name":"x","version":"1","protocol":"loomux-plugin/1","future":{"a":1}}`)
	if _, err := ParseManifest(data); err != nil {
		t.Fatalf("a newer manifest's unknown field should be ignored: %v", err)
	}
}

func TestSchemaValidate(t *testing.T) {
	s, err := okManifest(t).Schema()
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{
		"namespace": "loomux-agents", "agent_image": "ghcr.io/loomux/agent:0.4.0", "kubeconfig": "yaml",
		"egress": "none", "max_environments": float64(3), "require_network_policy": true,
		"sizes": map[string]any{"small": map[string]any{"cpu": "1", "memory": "2Gi"}},
	}
	if err := s.Validate(good); err != nil {
		t.Fatalf("Validate(good): %v", err)
	}
	bad := map[string]struct {
		cfg   map[string]any
		field string
	}{
		"missing required": {map[string]any{"namespace": "x"}, "agent_image"},
		"wrong type":       {map[string]any{"namespace": 3, "agent_image": "i"}, "namespace"},
		"enum":             {map[string]any{"namespace": "x", "agent_image": "i", "egress": "lan"}, "egress"},
		"minimum":          {map[string]any{"namespace": "x", "agent_image": "i", "max_environments": float64(0)}, "max_environments"},
		"not integer":      {map[string]any{"namespace": "x", "agent_image": "i", "max_environments": 1.5}, "max_environments"},
		"unknown key":      {map[string]any{"namespace": "x", "agent_image": "i", "colour": "red"}, "colour"},
		"nested type":      {map[string]any{"namespace": "x", "agent_image": "i", "sizes": map[string]any{"small": map[string]any{"cpu": 1}}}, "sizes.small.cpu"},
		"nested unknown":   {map[string]any{"namespace": "x", "agent_image": "i", "sizes": map[string]any{"small": map[string]any{"gpu": "1"}}}, "sizes.small.gpu"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			err := s.Validate(c.cfg)
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("want *ConfigError, got %v", err)
			}
			if ce.Field != c.field {
				t.Errorf("field = %q, want %q (%v)", ce.Field, c.field, err)
			}
			if strings.Contains(err.Error(), "yaml") {
				t.Error("an error must not echo a value")
			}
		})
	}
}

func TestSchemaSecretsDefaultsSplit(t *testing.T) {
	s, err := okManifest(t).Schema()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.SecretFields(); len(got) != 1 || got[0] != "kubeconfig" {
		t.Errorf("SecretFields = %v", got)
	}
	cfg := s.ApplyDefaults(map[string]any{"agent_image": "i", "egress": "none"})
	if cfg["namespace"] != "loomux-agents" || cfg["max_environments"] != float64(5) || cfg["egress"] != "none" || cfg["require_network_policy"] != false {
		t.Errorf("ApplyDefaults = %v", cfg)
	}
	if _, ok := cfg["kubeconfig"]; ok {
		t.Error("a field without a default must stay absent")
	}
	plain, secrets := s.Split(map[string]any{"namespace": "n", "kubeconfig": "yaml"})
	if secrets["kubeconfig"] != "yaml" || len(secrets) != 1 {
		t.Errorf("secrets = %v", secrets)
	}
	if _, ok := plain["kubeconfig"]; ok || plain["namespace"] != "n" {
		t.Errorf("plain = %v", plain)
	}
	merged := MergeSecrets(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "", "c": "3"})
	if merged["a"] != "1" || merged["c"] != "3" || len(merged) != 2 {
		t.Errorf("MergeSecrets = %v", merged)
	}
}

func TestEmptySchema(t *testing.T) {
	m := &Manifest{Name: "x", Version: "1", Protocol: ProtocolV1}
	s, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(map[string]any{}); err != nil {
		t.Errorf("empty config against empty schema: %v", err)
	}
	if err := s.Validate(map[string]any{"a": 1}); err == nil {
		t.Error("an empty schema must refuse any key")
	}
}

func manifestWith(t *testing.T, edits string) *Manifest {
	t.Helper()
	m, err := ParseManifest([]byte(`{"name":"x","title":"X","version":"0.3.0","protocol":"loomux-plugin/1",
		"capabilities":["targets.create","targets.stop_start"],
		"config_schema":{"type":"object","properties":{"a":{"type":"string"}}}` + edits + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// HostCompatible: min_host_version is a caret range on the host's
// version (LOOM-194); a host without a version (dev, a commit) runs
// anything, and so does a manifest without a minimum.
// HostCompatible (user decision 2026-10-11): refused when the host is
// below min_host_version or on another major (from 1.0); a plugin built
// for an older 0.Y on a newer 0.x host is marked and warned about, and
// runs.
func TestManifestHostCompatible(t *testing.T) {
	const (
		ok     = "ok"
		warn   = "warn"
		refuse = "refuse"
	)
	for _, tc := range []struct {
		min, host string
		want      string
	}{
		{"", "0.3.80", ok},
		{"0.3.0", "0.3.80 (web 0.3.12)", ok},
		{"0.3.81", "0.3.80", refuse},
		{"0.4.0", "0.3.80", refuse},
		{"0.3.0", "0.4.0", warn},
		{"0.3.0", "0.9.9", warn},
		{"1.0.0", "1.2.3", ok},
		{"1.0.0", "2.0.0", refuse},
		{"0.3.0", "1.0.0", refuse},
		{"9.0.0", "dev", ok},
		{"9.0.0", "abc1234 (web 0.3.1)", ok},
		{"9.0.0", "", ok},
	} {
		m := manifestWith(t, "")
		m.MinHostVersion = tc.min
		err := m.HostCompatible(tc.host)
		got := ok
		var inc *IncompatibleError
		switch {
		case err == nil:
		case errors.As(err, &inc) && inc.Refuse:
			got = refuse
		case errors.As(err, &inc):
			got = warn
		default:
			t.Errorf("min %q on host %q: %T %v, want *IncompatibleError", tc.min, tc.host, err, err)
			continue
		}
		if got != tc.want {
			t.Errorf("min %q on host %q = %s (%v), want %s", tc.min, tc.host, got, err, tc.want)
		}
	}
	m := manifestWith(t, "")
	m.MinHostVersion = "0.4.0"
	if err := m.HostCompatible("0.3.80 (web 0.3.12)"); err == nil ||
		!strings.Contains(err.Error(), "x 0.3.0 needs Loomux 0.4.x") || !strings.Contains(err.Error(), "this server is 0.3.80") {
		t.Errorf("refusal reason = %v", err)
	}
	m.MinHostVersion = "0.3.0"
	if err := m.HostCompatible("0.4.2"); err == nil ||
		!strings.Contains(err.Error(), "x 0.3.0 was built for Loomux 0.3.x") || !strings.Contains(err.Error(), "this server is 0.4.2") || !strings.Contains(err.Error(), "runs") {
		t.Errorf("warning reason = %v", err)
	}
}

func TestManifestMinHostVersionValidated(t *testing.T) {
	for _, bad := range []string{"0.4", "v0.4.0", "latest"} {
		if _, err := ParseManifest([]byte(`{"name":"x","version":"0.3.0","protocol":"loomux-plugin/1","min_host_version":"` + bad + `"}`)); err == nil {
			t.Errorf("min_host_version %q accepted", bad)
		}
	}
	if _, err := ParseManifest([]byte(`{"name":"x","version":"0.3.0","protocol":"loomux-plugin/1","min_host_version":"0.4.0"}`)); err != nil {
		t.Errorf("min_host_version 0.4.0 refused: %v", err)
	}
}

// SameCapabilitiesAndSchema is what lets the host adopt a bundled
// plugin's new version silently (D3): the version may differ, the
// capabilities (as a set) and the configuration schema may not.
func TestManifestSameCapabilitiesAndSchema(t *testing.T) {
	a := manifestWith(t, "")
	b := manifestWith(t, "")
	b.Version = "0.4.0"
	b.Description = "reworded"
	if !a.SameCapabilitiesAndSchema(b) {
		t.Error("a version and description change must count as the same")
	}
	b.Capabilities = []string{"targets.stop_start", "targets.create"}
	if !a.SameCapabilitiesAndSchema(b) {
		t.Error("capability order must not matter")
	}
	b.Capabilities = []string{"targets.create"}
	if a.SameCapabilitiesAndSchema(b) {
		t.Error("a dropped capability must differ")
	}
	c := manifestWith(t, "")
	c.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}}}`)
	if a.SameCapabilitiesAndSchema(c) {
		t.Error("a new setting must differ")
	}
	d := manifestWith(t, "")
	d.ConfigSchema = json.RawMessage(`{ "properties": {"a": {"type": "string"}}, "type": "object" }`)
	if !a.SameCapabilitiesAndSchema(d) {
		t.Error("the same schema with other key order and spacing must be the same")
	}
	e := manifestWith(t, "")
	e.ConfigSchema = nil
	f := manifestWith(t, "")
	f.ConfigSchema = json.RawMessage(`null`)
	if !e.SameCapabilitiesAndSchema(f) || e.SameCapabilitiesAndSchema(a) {
		t.Error("an absent schema equals null and differs from a real one")
	}
}

// A default is not part of the schema's shape: a stamped build changes
// the agent_image default with every agent rebuild, and that must not
// read as "changed its configuration schema" (review of #352). A
// type, a property or the required list still does.
func TestManifestSameSchemaIgnoresDefaults(t *testing.T) {
	a := manifestWith(t, "")
	a.ConfigSchema = json.RawMessage(`{"type":"object","required":["agent_image"],"properties":{
		"agent_image":{"type":"string","x-format":"image-reference","default":"ghcr.io/loomux/agent:main"},
		"sizes":{"type":"object","additionalProperties":{"type":"object","properties":{"cpu":{"type":"string","default":"1"}}}}}}`)
	b := manifestWith(t, "")
	b.ConfigSchema = json.RawMessage(`{"type":"object","required":["agent_image"],"properties":{
		"agent_image":{"type":"string","x-format":"image-reference","default":"ghcr.io/loomux/agent:0.3.90@sha256:abc"},
		"sizes":{"type":"object","additionalProperties":{"type":"object","properties":{"cpu":{"type":"string","default":"2"}}}}}}`)
	if !a.SameCapabilitiesAndSchema(b) {
		t.Error("a changed default (top level and nested) must still be the same schema")
	}
	c := manifestWith(t, "")
	c.ConfigSchema = json.RawMessage(`{"type":"object","required":["agent_image"],"properties":{
		"agent_image":{"type":"integer","x-format":"image-reference","default":"ghcr.io/loomux/agent:main"},
		"sizes":{"type":"object","additionalProperties":{"type":"object","properties":{"cpu":{"type":"string","default":"1"}}}}}}`)
	if a.SameCapabilitiesAndSchema(c) {
		t.Error("a changed type must differ")
	}
	d := manifestWith(t, "")
	d.ConfigSchema = json.RawMessage(`{"type":"object","required":[],"properties":{
		"agent_image":{"type":"string","x-format":"image-reference","default":"ghcr.io/loomux/agent:main"},
		"sizes":{"type":"object","additionalProperties":{"type":"object","properties":{"cpu":{"type":"string","default":"1"}}}}}}`)
	if a.SameCapabilitiesAndSchema(d) {
		t.Error("a changed required list must differ")
	}
	e := manifestWith(t, "")
	e.ConfigSchema = json.RawMessage(`{"type":"object","required":["agent_image"],"properties":{
		"agent_image":{"type":"string","x-format":"image-reference","default":"ghcr.io/loomux/agent:main"},
		"sizes":{"type":"object","additionalProperties":{"type":"object","properties":{"cpu":{"type":"string","default":"1"}}}},
		"timezone":{"type":"string","default":"UTC"}}}`)
	if a.SameCapabilitiesAndSchema(e) {
		t.Error("a new property must differ")
	}
}

// FollowDefaults: a stored setting that still equals the previous
// schema's default moves to the new default; one the operator set, or
// one whose default didn't change, stays.
func TestSchemaFollowDefaults(t *testing.T) {
	old := manifestWith(t, "")
	old.ConfigSchema = json.RawMessage(`{"type":"object","properties":{
		"agent_image":{"type":"string","default":"ghcr.io/loomux/agent:main"},
		"timezone":{"type":"string","default":"UTC"},
		"max":{"type":"integer","default":5},
		"token":{"type":"string","x-secret":true}}}`)
	cur := manifestWith(t, "")
	cur.ConfigSchema = json.RawMessage(`{"type":"object","properties":{
		"agent_image":{"type":"string","default":"ghcr.io/loomux/agent:0.3.90@sha256:abc"},
		"timezone":{"type":"string","default":"UTC"},
		"max":{"type":"integer","default":9},
		"token":{"type":"string","x-secret":true}}}`)
	oldS, _ := old.Schema()
	curS, _ := cur.Schema()
	stored := map[string]any{"agent_image": "ghcr.io/loomux/agent:main", "timezone": "Europe/Berlin", "max": float64(7), "token": "t"}
	got := curS.FollowDefaults(stored, oldS)
	want := map[string]any{"agent_image": "ghcr.io/loomux/agent:0.3.90@sha256:abc", "timezone": "Europe/Berlin", "max": float64(7), "token": "t"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FollowDefaults = %v, want %v", got, want)
	}
	if stored["agent_image"] != "ghcr.io/loomux/agent:main" {
		t.Error("the stored map was changed in place")
	}
	custom := map[string]any{"agent_image": "registry.local/agent:1", "max": float64(5)}
	if got := curS.FollowDefaults(custom, oldS); got["agent_image"] != "registry.local/agent:1" || got["max"] != float64(9) {
		t.Errorf("custom value changed, or an untouched default didn't follow: %v", got)
	}
	if got := curS.FollowDefaults(nil, nil); len(got) != 0 {
		t.Errorf("nil in, nil out: %v", got)
	}
}
