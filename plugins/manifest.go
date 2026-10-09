// Package plugins is the host side of Loomux's plugin system (LOOM-178,
// docs/design/target-providers.md §1): the manifest, the catalog of
// available plugins, the supervisor of a running one, and the manager
// that installs, configures and runs them.
package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Loomux/server/plugins/protocol"
)

// ProtocolV1 is the protocol this host speaks.
const ProtocolV1 = protocol.Version

// hostProtocolMajor is the protocol major this host accepts.
const hostProtocolMajor = 1

// KnownCapabilities are the capabilities a manifest may declare. Each
// maps to UI affordances and host behaviour; the host refuses a method
// a plugin didn't declare.
var KnownCapabilities = []string{
	"targets.create",
	"targets.stop_start",
	"targets.recreate",
	"targets.persistent",
	"targets.ephemeral",
	"targets.egress_policy",
	"targets.attach_commands",
}

// manifestName is what a plugin's name may be: it names its directory,
// its executable (loomux-plugin-<name>) and its socket.
var manifestName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidPluginName reports whether name can name a plugin.
func ValidPluginName(name string) bool { return manifestName.MatchString(name) }

// Permission is one thing a plugin declares it needs on the
// infrastructure: for informed consent, shown before install.
type Permission struct {
	Scope  string `json:"scope"`
	Detail string `json:"detail"`
}

// Manifest describes a plugin (plugin.json beside its executable, or
// what plugin.describe returns).
type Manifest struct {
	Name           string          `json:"name"`
	Title          string          `json:"title"`
	Version        string          `json:"version"`
	Protocol       string          `json:"protocol"`
	Vendor         string          `json:"vendor"`
	Homepage       string          `json:"homepage"`
	Description    string          `json:"description"`
	MinHostVersion string          `json:"min_host_version"`
	Capabilities   []string        `json:"capabilities"`
	Permissions    []Permission    `json:"permissions"`
	HostRequests   []string        `json:"host_requests"`
	ConfigSchema   json.RawMessage `json:"config_schema,omitempty"`
}

// ParseManifest reads and validates a manifest. Unknown fields are
// ignored: a newer plugin may carry more than this host knows.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("plugins: manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate refuses a manifest the host can't accept. Its errors are
// safe to show.
func (m *Manifest) Validate() error {
	if !ValidPluginName(m.Name) {
		return fmt.Errorf("plugins: manifest name %q: lowercase letters, digits and '-' only, at most 32, starting with a letter", m.Name)
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("plugins: manifest has no version")
	}
	major, err := m.ProtocolMajor()
	if err != nil {
		return err
	}
	if major != hostProtocolMajor {
		return fmt.Errorf("plugins: manifest %q speaks %s; this server speaks %s", m.Name, m.Protocol, ProtocolV1)
	}
	if len(m.HostRequests) != 0 {
		return fmt.Errorf("plugins: manifest %q asks the host for %v; no host request is supported", m.Name, m.HostRequests)
	}
	seen := map[string]bool{}
	for _, c := range m.Capabilities {
		if !knownCapability(c) {
			return fmt.Errorf("plugins: manifest %q declares unknown capability %q", m.Name, c)
		}
		if seen[c] {
			return fmt.Errorf("plugins: manifest %q declares capability %q twice", m.Name, c)
		}
		seen[c] = true
	}
	if _, err := m.Schema(); err != nil {
		return err
	}
	return nil
}

func knownCapability(c string) bool {
	for _, k := range KnownCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// HasCapability reports whether the manifest declares c.
func (m *Manifest) HasCapability(c string) bool {
	for _, have := range m.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// ProtocolMajor parses the manifest's protocol ("loomux-plugin/1").
func (m *Manifest) ProtocolMajor() (int, error) {
	rest, ok := strings.CutPrefix(m.Protocol, "loomux-plugin/")
	if !ok {
		return 0, fmt.Errorf("plugins: manifest %q protocol %q isn't loomux-plugin/<n>", m.Name, m.Protocol)
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("plugins: manifest %q protocol %q isn't loomux-plugin/<n>", m.Name, m.Protocol)
	}
	return n, nil
}

// Equal reports whether two manifests agree on what the host compares
// at the handshake: name, version, protocol and capabilities.
func (m *Manifest) Equal(o *Manifest) bool {
	if m.Name != o.Name || m.Version != o.Version || m.Protocol != o.Protocol || len(m.Capabilities) != len(o.Capabilities) {
		return false
	}
	for i := range m.Capabilities {
		if m.Capabilities[i] != o.Capabilities[i] {
			return false
		}
	}
	return true
}

// Schema is the subset of JSON Schema a manifest's config_schema may
// use: an object of string, integer, number, boolean and object
// properties, with required, enum, default, x-secret and x-format. The
// web renders it as a form.
type Schema struct {
	Type       string               `json:"type"`
	Properties map[string]*Property `json:"properties"`
	Required   []string             `json:"required"`
	// AdditionalProperties, when set, is the schema of any key not in
	// Properties (a map of sizes); nil refuses unknown keys.
	AdditionalProperties *Property `json:"additionalProperties"`
}

// Property is one schema property.
type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Default     any      `json:"default"`
	Enum        []any    `json:"enum"`
	Minimum     *float64 `json:"minimum"`
	// Secret fields are stored encrypted, never returned, and shown as
	// set/not set. Only a string may be secret, and only at the top
	// level.
	Secret bool `json:"x-secret"`
	// Format tells the UI which widget to show: ssh-private-key,
	// kubeconfig, image-reference, socks5-url.
	Format               string               `json:"x-format"`
	Properties           map[string]*Property `json:"properties"`
	Required             []string             `json:"required"`
	AdditionalProperties *Property            `json:"additionalProperties"`
}

// maxSchemaDepth bounds nesting: an object of objects of scalars.
const maxSchemaDepth = 3

// Schema returns the manifest's configuration schema; an absent one is
// an empty object (no configuration).
func (m *Manifest) Schema() (*Schema, error) {
	s := &Schema{Type: "object", Properties: map[string]*Property{}}
	if len(m.ConfigSchema) == 0 || string(m.ConfigSchema) == "null" {
		return s, nil
	}
	if err := json.Unmarshal(m.ConfigSchema, s); err != nil {
		return nil, fmt.Errorf("plugins: manifest %q config_schema: %w", m.Name, err)
	}
	if s.Type != "object" {
		return nil, fmt.Errorf("plugins: manifest %q config_schema must be an object, not %q", m.Name, s.Type)
	}
	if s.Properties == nil {
		s.Properties = map[string]*Property{}
	}
	root := &Property{Type: "object", Properties: s.Properties, Required: s.Required, AdditionalProperties: s.AdditionalProperties}
	if err := root.validate("", 0); err != nil {
		return nil, fmt.Errorf("plugins: manifest %q config_schema: %w", m.Name, err)
	}
	return s, nil
}

func (p *Property) validate(path string, depth int) error {
	switch p.Type {
	case "string", "integer", "number", "boolean":
	case "object":
		if depth >= maxSchemaDepth {
			return fmt.Errorf("%s: objects nest too deep (at most %d)", path, maxSchemaDepth)
		}
		for name, child := range p.Properties {
			if child == nil {
				return fmt.Errorf("%s.%s: empty property", path, name)
			}
			if err := child.validate(path+"."+name, depth+1); err != nil {
				return err
			}
			if child.Secret && depth > 0 {
				return fmt.Errorf("%s.%s: x-secret is only allowed at the top level", path, name)
			}
		}
		for _, r := range p.Required {
			if _, ok := p.Properties[r]; !ok {
				return fmt.Errorf("%s: required %q isn't a property", path, r)
			}
		}
		if p.AdditionalProperties != nil {
			if err := p.AdditionalProperties.validate(path+".*", depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: unsupported type %q", path, p.Type)
	}
	if p.Secret && p.Type != "string" {
		return fmt.Errorf("%s: x-secret must be a string", path)
	}
	if p.Default != nil {
		if err := p.check(path, p.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// ConfigError says which configuration field is wrong and how; safe to
// show.
type ConfigError struct {
	Field string
	Msg   string
}

func (e *ConfigError) Error() string {
	if e.Field == "" {
		return "config: " + e.Msg
	}
	return "config." + e.Field + ": " + e.Msg
}

// Validate refuses a configuration the schema doesn't describe: a
// missing required field, a wrong type, a value outside its enum or
// minimum, or a key the schema doesn't name.
func (s *Schema) Validate(config map[string]any) error {
	root := &Property{Type: "object", Properties: s.Properties, Required: s.Required, AdditionalProperties: s.AdditionalProperties}
	return root.check("", config)
}

func (p *Property) check(path string, v any) error {
	field := strings.TrimPrefix(path, ".")
	switch p.Type {
	case "string":
		if _, ok := v.(string); !ok {
			return &ConfigError{field, "must be a string"}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return &ConfigError{field, "must be true or false"}
		}
	case "integer", "number":
		f, ok := asNumber(v)
		if !ok {
			return &ConfigError{field, "must be a number"}
		}
		if p.Type == "integer" && f != math.Trunc(f) {
			return &ConfigError{field, "must be a whole number"}
		}
		if p.Minimum != nil && f < *p.Minimum {
			return &ConfigError{field, fmt.Sprintf("must be at least %v", *p.Minimum)}
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return &ConfigError{field, "must be an object"}
		}
		for _, r := range p.Required {
			if _, ok := obj[r]; !ok {
				return &ConfigError{strings.TrimPrefix(path+"."+r, "."), "is required"}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child, ok := p.Properties[k]
			if !ok {
				child = p.AdditionalProperties
			}
			if child == nil {
				return &ConfigError{strings.TrimPrefix(path+"."+k, "."), "isn't a setting of this plugin"}
			}
			if err := child.check(path+"."+k, obj[k]); err != nil {
				return err
			}
		}
	}
	if len(p.Enum) > 0 && !inEnum(p.Enum, v) {
		return &ConfigError{field, "must be one of " + enumList(p.Enum)}
	}
	return nil
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = fmt.Sprint(e)
	}
	return strings.Join(parts, ", ")
}

// SecretFields are the top-level fields marked x-secret, sorted.
func (s *Schema) SecretFields() []string {
	var out []string
	for name, p := range s.Properties {
		if p != nil && p.Secret {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Split separates a validated configuration into its plain part and its
// secret fields (as strings; the schema makes them strings).
func (s *Schema) Split(config map[string]any) (plain map[string]any, secrets map[string]string) {
	plain = map[string]any{}
	secrets = map[string]string{}
	secretNames := map[string]bool{}
	for _, n := range s.SecretFields() {
		secretNames[n] = true
	}
	for k, v := range config {
		if secretNames[k] {
			if str, ok := v.(string); ok {
				secrets[k] = str
			}
			continue
		}
		plain[k] = v
	}
	return plain, secrets
}

// ApplyDefaults returns a copy of config with every absent top-level
// field that has a default filled in.
func (s *Schema) ApplyDefaults(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		out[k] = v
	}
	for name, p := range s.Properties {
		if p == nil || p.Default == nil {
			continue
		}
		if _, ok := out[name]; !ok {
			out[name] = p.Default
		}
	}
	return out
}

// Merge returns stored with the keys of update written over it (an
// update's secret omitted keeps the stored one; "" clears it).
func MergeSecrets(stored, update map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range update {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
