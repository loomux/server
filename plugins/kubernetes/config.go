package kubernetes

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// Config is the plugin's configuration, parsed from what the host sends
// at plugin.configure (the manifest's config_schema, defaults applied).
type Config struct {
	Namespace            string
	StorageClass         string
	Subdomain            string
	AgentImage           string
	Kubeconfig           string // a secret; empty in-cluster
	Timezone             string
	Sizes                []protocol.Size
	MaxEnvironments      int
	RequireNetworkPolicy bool
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

var sizeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// defaultSizes is what the plugin offers unless "sizes" says otherwise.
var defaultSizes = []protocol.Size{
	{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"},
	{Name: "medium", CPU: "2", Memory: "4Gi", Disk: "20Gi"},
	{Name: "large", CPU: "4", Memory: "8Gi", Disk: "40Gi"},
}

func invalid(format string, args ...any) error {
	return &rpc.Error{Code: rpc.CodeInvalidConfig, Message: fmt.Sprintf(format, args...)}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// parseConfig validates the configuration. Its errors are invalid_config
// and never repeat the kubeconfig.
func parseConfig(m map[string]any) (Config, error) {
	c := Config{
		Namespace: str(m, "namespace"), StorageClass: str(m, "storage_class"), Subdomain: str(m, "subdomain"),
		AgentImage: str(m, "agent_image"), Kubeconfig: str(m, "kubeconfig"), Timezone: str(m, "timezone"),
		MaxEnvironments: 5,
	}
	for _, f := range []struct{ name, v string }{{"namespace", c.Namespace}, {"subdomain", c.Subdomain}} {
		if !dnsLabel.MatchString(f.v) {
			return c, invalid("%s must be a DNS label: lower-case letters, digits and dashes", f.name)
		}
	}
	if c.StorageClass == "" {
		return c, invalid("storage_class is required")
	}
	if c.AgentImage == "" || strings.ContainsAny(c.AgentImage, " \t\r\n\"'`$") {
		return c, invalid("agent_image must be an image reference")
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if len(c.Timezone) > 64 || strings.ContainsAny(c.Timezone, " \t\r\n\"'`$") {
		return c, invalid("timezone must be a tz name such as Europe/Warsaw")
	}
	switch v := m["max_environments"].(type) {
	case nil:
	case float64:
		c.MaxEnvironments = int(v)
	case int:
		c.MaxEnvironments = v
	default:
		return c, invalid("max_environments must be an integer")
	}
	if c.MaxEnvironments < 1 {
		return c, invalid("max_environments must be at least 1")
	}
	if v, ok := m["require_network_policy"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return c, invalid("require_network_policy must be true or false")
		}
		c.RequireNetworkPolicy = b
	}
	sizes, err := parseSizes(m["sizes"])
	if err != nil {
		return c, err
	}
	c.Sizes = sizes
	return c, nil
}

func parseSizes(v any) ([]protocol.Size, error) {
	m, ok := v.(map[string]any)
	if v != nil && !ok {
		return nil, invalid("sizes must be an object of {cpu, memory, disk} by name")
	}
	if len(m) == 0 {
		return append([]protocol.Size(nil), defaultSizes...), nil
	}
	out := make([]protocol.Size, 0, len(m))
	for name, raw := range m {
		if !sizeName.MatchString(name) {
			return nil, invalid("size %q: the name must be lower-case letters, digits and dashes", name)
		}
		fields, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid("size %q must be an object of {cpu, memory, disk}", name)
		}
		s := protocol.Size{Name: name, CPU: str(fields, "cpu"), Memory: str(fields, "memory"), Disk: str(fields, "disk")}
		for _, q := range []struct{ k, v string }{{"cpu", s.CPU}, {"memory", s.Memory}, {"disk", s.Disk}} {
			if q.v == "" {
				return nil, invalid("size %q: %s is required", name, q.k)
			}
			if _, err := resource.ParseQuantity(q.v); err != nil {
				return nil, invalid("size %q: %s %q isn't a Kubernetes quantity", name, q.k, q.v)
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := resource.MustParse(out[i].CPU), resource.MustParse(out[j].CPU)
		if c := ci.Cmp(cj); c != 0 {
			return c < 0
		}
		mi, mj := resource.MustParse(out[i].Memory), resource.MustParse(out[j].Memory)
		if c := mi.Cmp(mj); c != 0 {
			return c < 0
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// size is the named size, if offered.
func (c Config) size(name string) (protocol.Size, bool) {
	for _, s := range c.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return protocol.Size{}, false
}

func (c Config) sizeNames() string {
	names := make([]string, 0, len(c.Sizes))
	for _, s := range c.Sizes {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// addressTemplate is where machines are reachable: the pod's stable
// name through the headless Service (design §2.3).
func (c Config) addressTemplate() string {
	return "lx-{id}." + c.Subdomain + "." + c.Namespace + ".svc.cluster.local"
}

func (c Config) address(id string) protocol.Address {
	return protocol.Address{Host: strings.Replace(c.addressTemplate(), "{id}", id, 1), Port: sshPort, Proxy: protocol.SSHProxyNone}
}
