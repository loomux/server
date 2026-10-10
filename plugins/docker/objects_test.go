package docker

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := parseConfig(map[string]any{"engine": "unix:///var/run/docker.sock", "bind_address": "100.64.0.5", "agent_image": "ghcr.io/loomux/agent:test", "timezone": "Europe/Warsaw"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The container of design §9, with what Docker made necessary (the
// plan's findings): every security field, nothing from the host.
func TestContainerConfig(t *testing.T) {
	cfg := testConfig(t)
	sp := spec("k7f3q2")
	size, _ := cfg.size("small")
	c := containerFor(sp, size, cfg, testInstance, 32768)

	if c.Image != sp.Image || c.User != "10002:10002" || c.Hostname != "lx-k7f3q2" {
		t.Errorf("image/user/hostname = %q %q %q", c.Image, c.User, c.Hostname)
	}
	env := strings.Join(c.Env, "\n")
	for _, want := range []string{"HOME=/data/home", "CLAUDE_CONFIG_DIR=/data/home/.claude", "TZ=Europe/Warsaw"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, c.Env)
		}
	}
	if strings.Contains(env, "LOOMUX_") {
		t.Errorf("a LOOMUX_* variable reaches the machine: %q", c.Env)
	}
	if _, ok := c.ExposedPorts["2222/tcp"]; !ok || len(c.ExposedPorts) != 1 {
		t.Errorf("exposed ports = %v", c.ExposedPorts)
	}
	if c.Healthcheck == nil || strings.Join(c.Healthcheck.Test, " ") != "CMD bash -c exec 3<>/dev/tcp/127.0.0.1/2222" || c.Healthcheck.Interval != 5*time.Second || c.Healthcheck.Retries != 3 {
		t.Errorf("healthcheck = %+v", c.Healthcheck)
	}
	h := c.HostConfig
	if h.NetworkMode != "loomux-agents" || h.RestartPolicy.Name != "unless-stopped" || !h.ReadonlyRootfs || h.Init == nil || !*h.Init {
		t.Errorf("network/restart/readonly/init = %q %q %v %v", h.NetworkMode, h.RestartPolicy.Name, h.ReadonlyRootfs, h.Init)
	}
	if pb := h.PortBindings["2222/tcp"]; len(pb) != 1 || pb[0].HostIP != "100.64.0.5" || pb[0].HostPort != "32768" || len(h.PortBindings) != 1 {
		t.Errorf("port bindings = %v", h.PortBindings)
	}
	if h.Tmpfs["/tmp"] != "size=1g" || h.Tmpfs["/run/loomux"] != "size=1m" || len(h.Tmpfs) != 2 {
		t.Errorf("tmpfs = %v", h.Tmpfs)
	}
	if strings.Join(h.CapDrop, ",") != "ALL" || strings.Join(h.SecurityOpt, ",") != "no-new-privileges:true" {
		t.Errorf("caps/security = %v %v", h.CapDrop, h.SecurityOpt)
	}
	if h.PidsLimit == nil || *h.PidsLimit != 512 {
		t.Errorf("pids limit = %v", h.PidsLimit)
	}
	if h.Memory != 2<<30 || h.MemorySwap != 2<<30 || h.NanoCPUs != 1e9 {
		t.Errorf("memory %d swap %d cpus %d", h.Memory, h.MemorySwap, h.NanoCPUs)
	}
	if len(h.Mounts) != 2 {
		t.Fatalf("mounts = %+v", h.Mounts)
	}
	byTarget := map[string]mount{}
	for _, m := range h.Mounts {
		byTarget[m.Target] = m
	}
	if m := byTarget["/etc/loomux/ssh-src"]; m.Type != "volume" || m.Source != "lx-k7f3q2-ssh" || !m.ReadOnly {
		t.Errorf("ssh mount = %+v", m)
	}
	if m := byTarget["/data"]; m.Type != "volume" || m.Source != "lx-k7f3q2-data" || m.ReadOnly {
		t.Errorf("data mount = %+v", m)
	}
	// Ephemeral: an anonymous volume, which goes with the container.
	sp.Persistent = false
	c2 := containerFor(sp, size, cfg, testInstance, 32768)
	for _, m := range c2.HostConfig.Mounts {
		if m.Target == "/data" && (m.Type != "volume" || m.Source != "") {
			t.Errorf("ephemeral data mount = %+v", m)
		}
	}
	// Nothing from the host, nothing privileged: asserted on the JSON
	// the engine receives, so a field added to the struct can't slip in.
	js, _ := json.Marshal(c)
	for _, forbidden := range []string{`"Privileged":true`, `"Binds":[`, `"CapAdd":[`, `"Devices":[`, `"PidMode":"host"`, `"IpcMode":"host"`, `"NetworkMode":"host"`, `"UsernsMode"`, `"type":"bind"`, `"Type":"bind"`, `docker.sock`} {
		if strings.Contains(string(js), forbidden) {
			t.Errorf("the container config carries %s: %s", forbidden, js)
		}
	}
	labels := c.Labels
	for k, want := range map[string]string{labelManagedBy: managedBy, protocol.LabelInstance: testInstance, protocol.LabelTarget: "target-k7f3q2", protocol.LabelEnvironment: "k7f3q2", labelRole: roleAgent, labelPort: "32768", labelName: "builds for project x"} {
		if labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, labels[k], want)
		}
	}
	if _, ok := labels[labelSpec]; ok {
		t.Error("the container carries the spec; the record does")
	}
}

// The helpers: one probes a port (publishing sshd's port, mounting
// nothing), one receives the record's files (the record mounted
// read-write); both sleep, hardened like the machine.
func TestHelperConfig(t *testing.T) {
	cfg := testConfig(t)
	sp := spec("k7f3q2")
	for _, mode := range []helperMode{helperProbe, helperUpload} {
		h := helperFor(sp.ID, sp.Image, cfg, testInstance, mode)
		if strings.Join(h.Entrypoint, " ") != "/bin/sleep" || strings.Join(h.Cmd, " ") != "infinity" || h.User != "10002:10002" || h.Healthcheck != nil {
			t.Errorf("helper = %+v", h)
		}
		if !h.HostConfig.ReadonlyRootfs || strings.Join(h.HostConfig.CapDrop, ",") != "ALL" || strings.Join(h.HostConfig.SecurityOpt, ",") != "no-new-privileges:true" || h.HostConfig.RestartPolicy.Name != "no" {
			t.Errorf("helper host config = %+v", h.HostConfig)
		}
		if h.Labels[labelRole] != roleInit || h.Labels[protocol.LabelInstance] != testInstance || h.Labels[protocol.LabelEnvironment] != "k7f3q2" {
			t.Errorf("helper labels = %v", h.Labels)
		}
	}
	probe := helperFor(sp.ID, sp.Image, cfg, testInstance, helperProbe)
	if pb := probe.HostConfig.PortBindings["2222/tcp"]; len(pb) != 1 || pb[0].HostIP != "100.64.0.5" || pb[0].HostPort != "0" {
		t.Errorf("probe bindings = %v", probe.HostConfig.PortBindings)
	}
	if len(probe.HostConfig.Mounts) != 0 {
		t.Errorf("the probe mounts %+v: the daemon would create the record without its labels", probe.HostConfig.Mounts)
	}
	upload := helperFor(sp.ID, sp.Image, cfg, testInstance, helperUpload)
	if len(upload.HostConfig.Mounts) != 1 || upload.HostConfig.Mounts[0].Source != "lx-k7f3q2-ssh" || upload.HostConfig.Mounts[0].Target != "/ssh" || upload.HostConfig.Mounts[0].ReadOnly {
		t.Errorf("upload mounts = %+v", upload.HostConfig.Mounts)
	}
	if len(upload.HostConfig.PortBindings) != 0 {
		t.Errorf("the upload helper publishes %v", upload.HostConfig.PortBindings)
	}
}

// The record (the ssh volume's labels) remembers the spec without key
// material, the port and the creation time.
func TestRecordLabels(t *testing.T) {
	sp := spec("k7f3q2")
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	labels := recordLabels(sp, testInstance, 32768, now)
	for _, secret := range []string{"TESTKEY", "PRIVATE KEY", "AAAAC3NzaC1lZDI1NTE5AAAAITest"} {
		for k, v := range labels {
			if strings.Contains(v, secret) {
				t.Errorf("label %s carries key material", k)
			}
		}
	}
	got, err := specFrom(labels)
	if err != nil {
		t.Fatal(err)
	}
	want := sp
	want.SSH = protocol.SSHBootstrap{Port: 2222}
	if got.ID != want.ID || got.Size != want.Size || got.Persistent != want.Persistent || got.Image != want.Image || got.SSH != want.SSH || got.Name != want.Name || got.TargetID != want.TargetID {
		t.Errorf("spec round trip = %+v", got)
	}
	if p := portFrom(labels); p != 32768 {
		t.Errorf("port = %d", p)
	}
	if c := createdFrom(labels, time.Now()); !c.Equal(now) {
		t.Errorf("created = %v", c)
	}
	if _, err := specFrom(map[string]string{labelSpec: "{"}); err == nil {
		t.Error("a damaged record should be an error")
	}
	if p := portFrom(map[string]string{}); p != 0 {
		t.Errorf("no port label → %d", p)
	}
}

// The archive holds sshd's two files, the agent's and readable only by
// it, as the Kubernetes Secret lands with fsGroup.
func TestSSHArchive(t *testing.T) {
	sp := spec("k7f3q2")
	var buf bytes.Buffer
	if err := writeSSHArchive(&buf, sp); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	files := map[string]*tar.Header{}
	contents := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name], contents[h.Name] = h, string(data)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	for _, name := range []string{"host_ed25519", "authorized_keys"} {
		h := files[name]
		if h == nil || h.Mode != 0o400 || h.Uid != 10002 || h.Gid != 10002 || h.Typeflag != tar.TypeReg {
			t.Errorf("%s = %+v", name, h)
		}
	}
	if contents["host_ed25519"] != sp.SSH.HostPrivateKey || contents["authorized_keys"] != sp.SSH.AuthorizedKey+"\n" {
		t.Errorf("contents = %q", contents)
	}
}

func TestValidateSpec(t *testing.T) {
	cfg := testConfig(t)
	host := protocol.HostInfo{InstanceID: testInstance}
	if err := validateSpec(spec("k7f3q2"), cfg, host); err != nil {
		t.Fatalf("a good spec: %v", err)
	}
	cases := map[string]func(s *protocol.EnvironmentSpec){
		"id uppercase":      func(s *protocol.EnvironmentSpec) { s.ID = "K7" },
		"id dash end":       func(s *protocol.EnvironmentSpec) { s.ID = "k7-" },
		"id long":           func(s *protocol.EnvironmentSpec) { s.ID = strings.Repeat("a", 41) },
		"id empty":          func(s *protocol.EnvironmentSpec) { s.ID = "" },
		"size unknown":      func(s *protocol.EnvironmentSpec) { s.Size = "huge" },
		"egress none":       func(s *protocol.EnvironmentSpec) { s.Egress = protocol.EgressNone },
		"egress other":      func(s *protocol.EnvironmentSpec) { s.Egress = "lan" },
		"image space":       func(s *protocol.EnvironmentSpec) { s.Image = "a b" },
		"no host key":       func(s *protocol.EnvironmentSpec) { s.SSH.HostPrivateKey = " " },
		"no authorized":     func(s *protocol.EnvironmentSpec) { s.SSH.AuthorizedKey = "" },
		"other instance":    func(s *protocol.EnvironmentSpec) { s.Labels[protocol.LabelInstance] = "someone-else" },
		"label newline":     func(s *protocol.EnvironmentSpec) { s.Labels["x"] = "a\nb" },
		"target id long":    func(s *protocol.EnvironmentSpec) { s.TargetID = strings.Repeat("t", 300) },
		"target id control": func(s *protocol.EnvironmentSpec) { s.TargetID = "a\x00b" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := spec("k7f3q2")
			mutate(&s)
			err := validateSpec(s, cfg, host)
			var rpcErr *rpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeInvalidParams {
				t.Errorf("want invalid_params, got %v", err)
			}
		})
	}
	if err := validateSpec(spec("k7f3q2"), cfg, protocol.HostInfo{}); err == nil {
		t.Error("a host without an instance id should be refused")
	}
}

func TestNamesAndPullQuery(t *testing.T) {
	if containerName("x") != "lx-x" || helperName("x") != "lx-x-init" || recordName("x") != "lx-x-ssh" || dataName("x") != "lx-x-data" {
		t.Error("names")
	}
	cases := map[string][2]string{
		"ghcr.io/loomux/agent:main":            {"ghcr.io/loomux/agent", "main"},
		"ghcr.io/loomux/agent@sha256:abc":      {"ghcr.io/loomux/agent", "sha256:abc"},
		"ghcr.io/loomux/agent":                 {"ghcr.io/loomux/agent", "latest"},
		"localhost:5000/agent":                 {"localhost:5000/agent", "latest"},
		"localhost:5000/agent:v1":              {"localhost:5000/agent", "v1"},
		"ghcr.io/loomux/agent:main@sha256:abc": {"ghcr.io/loomux/agent", "sha256:abc"},
	}
	for ref, want := range cases {
		q := pullQuery(ref)
		if q.Get("fromImage") != want[0] || q.Get("tag") != want[1] {
			t.Errorf("pullQuery(%q) = %v, want %v", ref, q, want)
		}
	}
}
