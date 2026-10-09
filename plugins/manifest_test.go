package plugins

import (
	"encoding/json"
	"errors"
	"os"
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
