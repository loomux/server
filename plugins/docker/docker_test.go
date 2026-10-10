//go:build docker

package docker_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/plugintest"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/targets/sshtest"
)

// TestDocker runs the plugin binary against a real engine (CI's
// runner, .github/workflows/plugins.yml; a workstation by hand):
//
//	LOOMUX_DOCKER_ENGINE=unix:///var/run/docker.sock LOOMUX_DOCKER_AGENT_IMAGE=loomux-agent:test \
//	  go test -tags docker -count=1 -timeout 15m -run TestDocker -v ./...
//
// The conformance suite, then end to end: create, wait for running,
// ssh in on 127.0.0.1 at the published port with the host key pinned,
// stop and start at the same port, recreate at the same port, destroy
// leaving nothing. Then the ssh transport for real: targets/sshtest
// serving this machine, whose shell runs docker system dial-stdio
// against the same engine (skipped without the docker CLI).
func TestDocker(t *testing.T) {
	engineAddr := os.Getenv("LOOMUX_DOCKER_ENGINE")
	if engineAddr == "" {
		t.Skip("LOOMUX_DOCKER_ENGINE not set")
	}
	image := os.Getenv("LOOMUX_DOCKER_AGENT_IMAGE")
	if image == "" {
		image = "loomux-agent:test"
	}
	bin := filepath.Join(t.TempDir(), "loomux-plugin-docker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/loomux-plugin-docker")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building the plugin: %v", err)
	}
	launch := func(t *testing.T) (*rpc.Conn, func()) {
		cmd := exec.Command(bin)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		conn := rpc.NewConn(stdout, stdin, nil)
		return conn, func() {
			conn.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	opts := plugintest.Options{
		Config:      map[string]any{"engine": engineAddr, "bind_address": "127.0.0.1", "ssh_proxy": "none", "agent_image": image},
		CallTimeout: 60 * time.Second,
	}
	t.Run("Conformance", func(t *testing.T) { plugintest.Run(t, launch, opts) })
	t.Run("EndToEnd", func(t *testing.T) { endToEnd(t, launch, opts, "docker-e2e") })
	t.Run("OverSSH", func(t *testing.T) {
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skip("no docker CLI: the shell can't run docker system dial-stdio")
		}
		if !strings.HasPrefix(engineAddr, "unix://") {
			t.Skip("the ssh form relays to this machine's own engine")
		}
		s := sshtest.Start(t)
		key, err := os.ReadFile(s.IdentityFile)
		if err != nil {
			t.Fatal(err)
		}
		sshOpts := opts
		sshOpts.Config = map[string]any{
			"engine": "ssh://" + os.Getenv("USER") + "@" + s.Host + ":" + itoa(s.Port), "ssh_private_key": string(key), "ssh_host_key": s.HostKey,
			"bind_address": "127.0.0.1", "ssh_proxy": "none", "agent_image": image,
		}
		unpinned := opts
		unpinned.Config = map[string]any{
			"engine": sshOpts.Config["engine"], "ssh_private_key": string(key),
			"bind_address": "127.0.0.1", "ssh_proxy": "none", "agent_image": image,
		}
		conn, stop := launch(t)
		defer stop()
		call := caller(conn, opts)
		var m plugins.Manifest
		if err := call(protocol.MethodDescribe, nil, &m); err != nil {
			t.Fatal(err)
		}
		schema, _ := m.Schema()
		if err := call(protocol.MethodConfigure, protocol.ConfigureParams{Config: schema.ApplyDefaults(unpinned.Config), Host: protocol.HostInfo{Version: "e2e", InstanceID: "docker-ssh"}}, nil); err != nil {
			t.Fatal(err)
		}
		var res protocol.CheckResult
		if err := call(protocol.MethodCheck, nil, &res); err != nil {
			t.Fatal(err)
		}
		if res.OK || !hasProblem(res, "host_key_unpinned") {
			t.Fatalf("unpinned check = %+v", res)
		}
		for _, p := range res.Problems {
			if p.Code == "host_key_unpinned" && !strings.Contains(p.Message, s.HostKey) {
				t.Errorf("the unpinned problem doesn't carry the key to trust: %s", p.Message)
			}
		}
		t.Run("Conformance", func(t *testing.T) { plugintest.Run(t, launch, sshOpts) })
		t.Run("EndToEnd", func(t *testing.T) { endToEnd(t, launch, sshOpts, "docker-ssh") })
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

func caller(conn *rpc.Conn, o plugintest.Options) func(method string, params, result any) error {
	return func(method string, params, result any) error {
		ctx, cancel := context.WithTimeout(context.Background(), o.CallTimeout)
		defer cancel()
		return conn.Call(ctx, method, params, result)
	}
}

func endToEnd(t *testing.T, launch plugintest.Launch, o plugintest.Options, instance string) {
	conn, stop := launch(t)
	defer stop()
	call := caller(conn, o)
	var m plugins.Manifest
	if err := call(protocol.MethodDescribe, nil, &m); err != nil {
		t.Fatal(err)
	}
	schema, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if err := call(protocol.MethodConfigure, protocol.ConfigureParams{Config: schema.ApplyDefaults(o.Config), Host: protocol.HostInfo{Version: "e2e", InstanceID: instance}}, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	var res protocol.CheckResult
	if err := call(protocol.MethodCheck, nil, &res); err != nil {
		t.Fatalf("check: %v", err)
	}
	t.Logf("check: ok=%v problems=%+v", res.OK, res.Problems)
	if !res.OK {
		t.Fatalf("check isn't ok: %+v", res.Problems)
	}

	// Keys, as the host makes them: the machine's host key, pinned
	// before it exists, and the target's key for authorized_keys.
	hostPub, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostBlock, _ := ssh.MarshalPrivateKey(hostPriv, "")
	hostSSHPub, _ := ssh.NewPublicKey(hostPub)
	tgtPub, tgtPriv, _ := ed25519.GenerateKey(rand.Reader)
	tgtBlock, _ := ssh.MarshalPrivateKey(tgtPriv, "")
	tgtSSHPub, _ := ssh.NewPublicKey(tgtPub)
	var info protocol.TargetsInfo
	if err := call(protocol.MethodTargetsDescribe, nil, &info); err != nil {
		t.Fatal(err)
	}
	if strings.Join(info.EgressOptions, ",") != protocol.EgressInternet {
		t.Errorf("egress options = %v: Docker can't confine a machine", info.EgressOptions)
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := "e2e-" + hex.EncodeToString(suffix)
	spec := protocol.EnvironmentSpec{
		ID: id, TargetID: "docker-target", Name: "docker e2e", Size: "small", Persistent: true, Egress: protocol.EgressInternet, Image: info.Image,
		SSH: protocol.SSHBootstrap{
			AuthorizedKey:  "no-port-forwarding,no-agent-forwarding,no-X11-forwarding " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(tgtSSHPub))),
			HostPrivateKey: string(pem.EncodeToMemory(hostBlock)),
			HostPublicKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSSHPub))),
			Port:           2222,
		},
		Labels: map[string]string{protocol.LabelInstance: instance, protocol.LabelTarget: "docker-target", protocol.LabelEnvironment: id},
	}
	defer func() { _ = call(protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil) }()
	var env protocol.Environment
	if err := call(protocol.MethodTargetsCreate, spec, &env); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s at %s:%d, %s %s", env.ID, env.Address.Host, env.Address.Port, env.Status, env.Reason)
	env = waitStatus(t, call, id, protocol.EnvRunning, 3*time.Minute)
	port := env.Address.Port
	if env.Address.Host != "127.0.0.1" || port < 1024 {
		t.Fatalf("address = %+v", env.Address)
	}
	var health protocol.EnvironmentHealth
	if err := call(protocol.MethodTargetsHealth, protocol.IDParams{ID: id}, &health); err != nil || health.Status != protocol.EnvRunning {
		t.Errorf("health = %+v, %v", health, err)
	}
	var att protocol.AttachResult
	if err := call(protocol.MethodTargetsAttachCommands, protocol.AttachParams{ID: id, Session: "loomux-e2e"}, &att); err != nil || len(att.Commands) != 1 || !strings.Contains(att.Commands[0].Command, "docker") {
		t.Errorf("attach = %+v, %v", att, err)
	}

	sshIn(t, port, tgtBlock, hostSSHPub)

	if err := call(protocol.MethodTargetsStop, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped := waitStatus(t, call, id, protocol.EnvStopped, 2*time.Minute)
	if stopped.Address.Port != port {
		t.Errorf("a stopped machine's port moved: %d → %d", port, stopped.Address.Port)
	}
	if err := call(protocol.MethodTargetsStart, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if started := waitStatus(t, call, id, protocol.EnvRunning, 3*time.Minute); started.Address.Port != port {
		t.Errorf("start moved the port: %d → %d", port, started.Address.Port)
	}
	sshIn(t, port, tgtBlock, hostSSHPub)

	spec2 := spec
	spec2.Size = "medium"
	var re protocol.Environment
	if err := call(protocol.MethodTargetsRecreate, protocol.RecreateParams{ID: id, Spec: spec2}, &re); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if re.Address.Port != port || re.Size != "medium" {
		t.Errorf("recreate = %+v, want port %d and size medium", re, port)
	}
	waitStatus(t, call, id, protocol.EnvRunning, 3*time.Minute)
	sshIn(t, port, tgtBlock, hostSSHPub)

	if err := call(protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := call(protocol.MethodTargetsGet, protocol.IDParams{ID: id}, &env); err == nil {
		t.Errorf("get after destroy = %+v, want not_found", env)
	}
	var list protocol.ListResult
	if err := call(protocol.MethodTargetsList, nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Environments {
		if e.ID == id {
			t.Errorf("destroyed machine still listed: %+v", e)
		}
	}
	nothingLeft(t, instance)
}

func hasProblem(res protocol.CheckResult, code string) bool {
	for _, p := range res.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

func waitStatus(t *testing.T, call func(string, any, any) error, id, want string, within time.Duration) protocol.Environment {
	t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for {
		// A fresh struct each time: an omitted reason must not linger.
		var env protocol.Environment
		if err := call(protocol.MethodTargetsGet, protocol.IDParams{ID: id}, &env); err != nil {
			t.Fatalf("get: %v", err)
		}
		if cur := env.Status + " " + env.Reason; cur != last {
			t.Logf("%s: %s", id, cur)
			last = cur
		}
		if env.Status == want {
			return env
		}
		var health protocol.EnvironmentHealth
		_ = call(protocol.MethodTargetsHealth, protocol.IDParams{ID: id}, &health)
		if env.Status == protocol.EnvError && want != protocol.EnvError {
			t.Fatalf("machine failed: %s (health: %s)", env.Reason, health.Reason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine is %s (%s), never %s within %s (health: %s %s)", env.Status, env.Reason, want, within, health.Status, health.Reason)
		}
		time.Sleep(2 * time.Second)
	}
}

// sshIn logs in the way Loomux does: the target's key, the host key
// pinned, at the published port on the bind address.
func sshIn(t *testing.T, port int, key *pem.Block, hostPub ssh.PublicKey) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Log("no ssh: skipping the ssh login")
		return
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(key), 0o600); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	p := itoa(port)
	if err := os.WriteFile(known, []byte("[127.0.0.1]:"+p+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub)))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var last []byte
	var err error
	for attempt := 0; attempt < 15; attempt++ {
		cmd := exec.Command("ssh", "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile="+known,
			"-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "LogLevel=ERROR",
			"-p", p, "agent@127.0.0.1", "tmux -V && id -u && test -w /data/work && cat /proc/1/comm && echo login-ok")
		last, err = cmd.CombinedOutput()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("ssh login at 127.0.0.1:%s: %v\n%s", p, err, last)
	}
	s := string(last)
	if !strings.Contains(s, "tmux ") || !strings.Contains(s, "\n10002\n") || !strings.Contains(s, "docker-init") || !strings.Contains(s, "login-ok") {
		t.Errorf("ssh output = %q", s)
	}
	t.Logf("ssh login ok: %s", strings.Join(strings.Fields(s), " "))
}

// nothingLeft asks the docker CLI (when there is one) for anything
// labelled as this instance's.
func nothingLeft(t *testing.T, instance string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return
	}
	for _, args := range [][]string{
		{"ps", "-a", "-q", "--filter", "label=loomux.io/instance=" + instance},
		{"volume", "ls", "-q", "--filter", "label=loomux.io/instance=" + instance},
	} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Logf("docker %s: %v", strings.Join(args, " "), err)
			continue
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			t.Errorf("docker %s left: %s", strings.Join(args, " "), s)
		}
	}
}
