package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dev = `{
  "name": "kubernetes",
  "title": "Kubernetes",
  "version": "0.0.0-dev",
  "protocol": "loomux-plugin/1",
  "capabilities": ["targets.create"],
  "config_schema": {
    "type": "object",
    "required": ["namespace", "agent_image"],
    "properties": {
      "namespace": {"type": "string", "default": "loomux-agents"},
      "agent_image": {"type": "string", "x-format": "image-reference", "default": "ghcr.io/loomux/agent:main", "description": "the image machines run"},
      "max_environments": {"type": "integer", "minimum": 1, "default": 5}
    }
  }
}
`

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("stamped output isn't JSON: %v\n%s", err, data)
	}
	return m
}

func agentDefault(m map[string]any) any {
	return m["config_schema"].(map[string]any)["properties"].(map[string]any)["agent_image"].(map[string]any)["default"]
}

func TestStampVersionAndMinHost(t *testing.T) {
	out, err := stamp([]byte(dev), "0.3.80", "")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["version"] != "0.3.80" || m["min_host_version"] != "0.3.0" {
		t.Errorf("version %v, min_host_version %v", m["version"], m["min_host_version"])
	}
	if agentDefault(m) != "ghcr.io/loomux/agent:main" {
		t.Errorf("agent_image default changed without one given: %v", agentDefault(m))
	}
	// The rest survives: the integer stays an integer, the description
	// stays, a trailing newline ends the file.
	if m["max_environments"] != nil || !strings.Contains(string(out), `"minimum": 1`) || !strings.Contains(string(out), "the image machines run") || !strings.HasSuffix(string(out), "\n") {
		t.Errorf("output = %s", out)
	}
}

func TestStampAgentImage(t *testing.T) {
	ref := "ghcr.io/loomux/agent:0.3.77@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	out, err := stamp([]byte(dev), "0.3.80", ref)
	if err != nil {
		t.Fatal(err)
	}
	if got := agentDefault(decode(t, out)); got != ref {
		t.Errorf("agent_image default = %v", got)
	}
	// A manifest without an agent_image setting is left alone.
	noAgent := strings.Replace(dev, `"agent_image": {"type": "string", "x-format": "image-reference", "default": "ghcr.io/loomux/agent:main", "description": "the image machines run"},`, "", 1)
	noAgent = strings.Replace(noAgent, `"required": ["namespace", "agent_image"],`, `"required": ["namespace"],`, 1)
	out, err = stamp([]byte(noAgent), "0.3.80", ref)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sha256:") {
		t.Errorf("an agent reference appeared in a manifest without the setting: %s", out)
	}
}

// A pre-release (a hand-pushed v1.0.0-rc.1) is a release too; its
// minimum is itself, so the host that carries it meets it and so does
// the release after it.
func TestStampPreRelease(t *testing.T) {
	out, err := stamp([]byte(dev), "1.0.0-rc.1", "")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["version"] != "1.0.0-rc.1" || m["min_host_version"] != "1.0.0-rc.1" {
		t.Errorf("version %v, min_host_version %v", m["version"], m["min_host_version"])
	}
}

func TestStampNothingToDoKeepsTheManifest(t *testing.T) {
	out, err := stamp([]byte(dev), "", "")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["version"] != "0.0.0-dev" || m["min_host_version"] != nil || agentDefault(m) != "ghcr.io/loomux/agent:main" {
		t.Errorf("an empty stamp changed the manifest: %s", out)
	}
}

func TestStampRefuses(t *testing.T) {
	for _, v := range []string{"0.3", "v0.3.1", "main", "0.3.1 (web 0.3.0)", "0.3.1+build.1"} {
		if _, err := stamp([]byte(dev), v, ""); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	if _, err := stamp([]byte(`{"name":"x"}`), "0.3.1", ""); err == nil {
		t.Error("a manifest without a version was stamped")
	}
	if _, err := stamp([]byte(`not json`), "0.3.1", ""); err == nil {
		t.Error("a non-JSON file was stamped")
	}
}

// run stamps files in place, as the Dockerfiles call it.
func TestRunStampsFilesInPlace(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(dev), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"-version", "0.3.80", "-agent-image", "ghcr.io/loomux/agent:0.3.77@sha256:abc", a, b}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b} {
		data, _ := os.ReadFile(p)
		m := decode(t, data)
		if m["version"] != "0.3.80" || m["min_host_version"] != "0.3.0" || agentDefault(m) != "ghcr.io/loomux/agent:0.3.77@sha256:abc" {
			t.Errorf("%s = %s", p, data)
		}
	}
	if err := run([]string{"-version", "0.3.80"}); err == nil {
		t.Error("no files should be an error")
	}
	if err := run([]string{"-version", "bad", a}); err == nil {
		t.Error("a bad version should be an error before any file is touched")
	}
}
