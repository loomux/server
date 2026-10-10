package docker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// testKey is a fresh ed25519 key: its private half in OpenSSH PEM (what
// the host's Generate button stores) and its public half as a line.
func testKey(t *testing.T) (pemText, publicLine string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

// baseConfig is a valid configuration with the schema's defaults
// applied, as the host sends it.
func baseConfig(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	key, _ := testKey(t)
	cfg := map[string]any{"engine": "ssh://docker@jet01.example:2222", "ssh_private_key": key, "bind_address": "100.64.0.5", "agent_image": "ghcr.io/loomux/agent:test"}
	for k, v := range extra {
		if v == nil {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
	}
	m, err := plugins.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	cfg = s.ApplyDefaults(cfg)
	if err := s.Validate(cfg); err != nil {
		t.Fatalf("config isn't valid for the schema: %v", err)
	}
	return cfg
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig(baseConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine.Kind != engineSSH || c.Engine.User != "docker" || c.Engine.Host != "jet01.example" || c.Engine.Port != 2222 {
		t.Errorf("engine = %+v", c.Engine)
	}
	if c.signer == nil {
		t.Error("the private key wasn't parsed")
	}
	if c.hostKey != nil {
		t.Error("no ssh_host_key given, yet one is pinned")
	}
	if c.BindAddress != "100.64.0.5" || c.SSHProxy != protocol.SSHProxyDefault || c.Timezone != "UTC" || c.MaxEnvironments != 5 || c.Proxy != "" {
		t.Errorf("config = %+v", c)
	}
	if c.AgentImage != "ghcr.io/loomux/agent:test" {
		t.Errorf("agent_image = %q", c.AgentImage)
	}
	if len(c.Sizes) != 3 || c.Sizes[0].Name != "small" || c.Sizes[2].Name != "large" {
		t.Errorf("sizes = %+v", c.Sizes)
	}
	if c.addressTemplate() != "100.64.0.5" {
		t.Errorf("address template = %q", c.addressTemplate())
	}
	if a := c.address(32768); a != (protocol.Address{Host: "100.64.0.5", Port: 32768, Proxy: protocol.SSHProxyDefault}) {
		t.Errorf("address = %+v", a)
	}
}

func TestParseConfigSSHDefaultPortAndUnixEngine(t *testing.T) {
	c, err := parseConfig(baseConfig(t, map[string]any{"engine": "ssh://root@10.0.0.9"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine.Port != 22 || c.Engine.Host != "10.0.0.9" {
		t.Errorf("engine = %+v", c.Engine)
	}
	// A unix engine needs no key and no pin; the proxy is for ssh only.
	c, err = parseConfig(baseConfig(t, map[string]any{"engine": "unix:///var/run/docker.sock", "ssh_private_key": nil, "bind_address": "127.0.0.1", "ssh_proxy": "none"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine.Kind != engineUnix || c.Engine.Path != "/var/run/docker.sock" || c.SSHProxy != protocol.SSHProxyNone {
		t.Errorf("engine = %+v, ssh_proxy %q", c.Engine, c.SSHProxy)
	}
}

func TestParseConfigHostKeyForms(t *testing.T) {
	_, pub := testKey(t)
	for _, line := range []string{pub, "[jet01.example]:2222 " + pub, "jet01.example " + pub + " a comment"} {
		c, err := parseConfig(baseConfig(t, map[string]any{"ssh_host_key": line}))
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		if c.hostKey == nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(c.hostKey))) != pub {
			t.Errorf("%q: pinned %v", line, c.hostKey)
		}
	}
}

func TestParseConfigProxyAndSizes(t *testing.T) {
	c, err := parseConfig(baseConfig(t, map[string]any{
		"proxy": "socks5://127.0.0.1:1055",
		"sizes": map[string]any{"big": map[string]any{"cpu": "4", "memory": "8Gi", "disk": "40Gi"}, "tiny": map[string]any{"cpu": "500m", "memory": "512Mi", "disk": "5G"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy != "127.0.0.1:1055" {
		t.Errorf("proxy = %q", c.Proxy)
	}
	if len(c.Sizes) != 2 || c.Sizes[0].Name != "tiny" || c.Sizes[1].Name != "big" {
		t.Errorf("sizes = %+v (smallest first)", c.Sizes)
	}
	if _, ok := c.size("tiny"); !ok {
		t.Error("size tiny not found")
	}
	if _, ok := c.size("small"); ok {
		t.Error("the default sizes should be replaced, not merged")
	}
}

func TestParseConfigRejects(t *testing.T) {
	key, pub := testKey(t)
	cases := map[string]map[string]any{
		"engine missing":          {"engine": "   "},
		"engine tcp":              {"engine": "tcp://10.0.0.9:2375"},
		"engine http":             {"engine": "http://10.0.0.9"},
		"engine ssh no user":      {"engine": "ssh://10.0.0.9"},
		"engine ssh bad port":     {"engine": "ssh://u@h:99999"},
		"engine ssh shell chars":  {"engine": "ssh://u@h;rm"},
		"engine unix relative":    {"engine": "unix://docker.sock"},
		"key missing for ssh":     {"ssh_private_key": nil},
		"key garbage":             {"ssh_private_key": "not a key"},
		"host key garbage":        {"ssh_host_key": "ssh-ed25519 not-base64"},
		"host key pub as private": {"ssh_private_key": pub},
		"bind missing":            {"bind_address": ""},
		"bind hostname":           {"bind_address": "jet01.example"},
		"bind unspecified v4":     {"bind_address": "0.0.0.0"},
		"bind unspecified v6":     {"bind_address": "::"},
		"bind with port":          {"bind_address": "100.64.0.5:22"},
		"proxy http":              {"proxy": "http://127.0.0.1:1055"},
		"proxy no port":           {"proxy": "socks5://127.0.0.1"},
		"proxy with user":         {"proxy": "socks5://u:p@127.0.0.1:1055"},
		"ssh_proxy":               {"ssh_proxy": "socks"},
		"image space":             {"agent_image": "ghcr.io/x y"},
		"max 0":                   {"max_environments": 0},
		"timezone shell":          {"timezone": "UTC; rm"},
		"size bad cpu":            {"sizes": map[string]any{"x": map[string]any{"cpu": "two", "memory": "1Gi", "disk": "1Gi"}}},
		"size bad memory":         {"sizes": map[string]any{"x": map[string]any{"cpu": "1", "memory": "1Gx", "disk": "1Gi"}}},
		"size missing disk":       {"sizes": map[string]any{"x": map[string]any{"cpu": "1", "memory": "1Gi"}}},
		"size bad name":           {"sizes": map[string]any{"Bad Name": map[string]any{"cpu": "1", "memory": "1Gi", "disk": "1Gi"}}},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := map[string]any{"engine": "ssh://docker@jet01.example", "ssh_private_key": key, "bind_address": "100.64.0.5", "agent_image": "ghcr.io/loomux/agent:test", "ssh_proxy": "default", "timezone": "UTC", "max_environments": float64(5)}
			for k, v := range extra {
				if v == nil {
					delete(cfg, k)
				} else {
					cfg[k] = v
				}
			}
			_, err := parseConfig(cfg)
			var rpcErr *rpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeInvalidConfig {
				t.Fatalf("want invalid_config, got %v", err)
			}
			if strings.Contains(rpcErr.Message, "PRIVATE KEY") || strings.Contains(rpcErr.Message, strings.TrimSpace(key)) {
				t.Errorf("the error repeats the private key: %s", rpcErr.Message)
			}
		})
	}
}

func TestQuantities(t *testing.T) {
	cpu := map[string]int64{"1": 1e9, "2": 2e9, "500m": 5e8, "0.5": 5e8, "1.5": 15e8, "250m": 25e7}
	for in, want := range cpu {
		got, err := parseCPU(in)
		if err != nil || got != want {
			t.Errorf("parseCPU(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "two", "1x", "-1", "1m1", "1e"} {
		if _, err := parseCPU(bad); err == nil {
			t.Errorf("parseCPU(%q) accepted", bad)
		}
	}
	bytes := map[string]int64{"2Gi": 2 << 30, "512Mi": 512 << 20, "1G": 1e9, "1500M": 15e8, "1Ti": 1 << 40, "1073741824": 1 << 30, "0.5Gi": 1 << 29, "64Ki": 64 << 10, "1k": 1000}
	for in, want := range bytes {
		got, err := parseBytes(in)
		if err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Gi", "1GiB", "1 Gi", "-1Gi", "1m"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("parseBytes(%q) accepted", bad)
		}
	}
}
