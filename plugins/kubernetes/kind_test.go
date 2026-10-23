//go:build kind

package kubernetes_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/plugintest"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// TestKind runs the plugin binary against a real cluster (CI's kind,
// .github/workflows/plugins.yml):
//
//	kind create cluster --config deploy/test/kind/cluster/kind.yaml --kubeconfig kind.kubeconfig
//	KUBECONFIG=kind.kubeconfig LOOMUX_KIND_KUBECONFIG=plugin.kubeconfig LOOMUX_KIND_AGENT_IMAGE=loomux-agent:test \
//	  go test -tags kind -count=1 -timeout 15m -run TestKind -v ./...
//
// LOOMUX_KIND_KUBECONFIG is the plugin ServiceAccount's (deploy/test/kind),
// so the Role's verbs are exactly what the plugin gets. kubectl (for a
// port-forward, as kind's admin) and ssh make the ssh login part run;
// without them it is skipped. KUBECONFIG must be kind's alone, current
// context kind-loomux, the plugin's cluster (kindKubectl), or the test
// fails before it starts; every kubectl gets that file and context
// spelled out, so none can reach the default context's cluster.
//
// LOOMUX_KIND_ENFORCED=1 (CI) says the cluster enforces NetworkPolicy,
// as kind's default CNI does, and the check must say enforced: the
// canary reaches kube-dns only. Without it the check must report
// network_policy_not_enforced, for a cluster with no policy engine.
// (CI once read kind as not enforced: the canary probed before the
// engine had learned its address; it retries now.)
func TestKind(t *testing.T) {
	kcPath := os.Getenv("LOOMUX_KIND_KUBECONFIG")
	if kcPath == "" {
		t.Skip("LOOMUX_KIND_KUBECONFIG not set")
	}
	kubeconfig, err := os.ReadFile(kcPath)
	if err != nil {
		t.Fatal(err)
	}
	kubectl, err := kindKubectl(os.Getenv("KUBECONFIG"), kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("LOOMUX_KIND_AGENT_IMAGE")
	if image == "" {
		image = "loomux-agent:test"
	}
	bin := filepath.Join(t.TempDir(), "loomux-plugin-kubernetes")
	build := exec.Command("go", "build", "-o", bin, "./cmd/loomux-plugin-kubernetes")
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
		Config:      map[string]any{"namespace": "loomux-agents", "storage_class": "standard", "agent_image": image, "kubeconfig": string(kubeconfig)},
		CallTimeout: 60 * time.Second,
	}
	t.Run("Conformance", func(t *testing.T) { plugintest.Run(t, launch, opts) })
	t.Run("EndToEnd", func(t *testing.T) { endToEnd(t, launch, opts, kubectl) })
}

func endToEnd(t *testing.T, launch plugintest.Launch, o plugintest.Options, kubectl []string) {
	conn, stop := launch(t)
	defer stop()
	call := func(method string, params, result any) error {
		ctx, cancel := context.WithTimeout(context.Background(), o.CallTimeout)
		defer cancel()
		return conn.Call(ctx, method, params, result)
	}
	var m plugins.Manifest
	if err := call(protocol.MethodDescribe, nil, &m); err != nil {
		t.Fatal(err)
	}
	schema, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	const instance = "kind-instance"
	if err := call(protocol.MethodConfigure, protocol.ConfigureParams{Config: schema.ApplyDefaults(o.Config), Host: protocol.HostInfo{Version: "kind", InstanceID: instance}}, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// The check: the Role suffices, the Service is there, and the
	// network-isolation verdict is what the cluster warrants.
	var res protocol.CheckResult
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if err := call(protocol.MethodCheck, nil, &res); err != nil {
			t.Fatalf("check: %v", err)
		}
		if !hasProblem(res, "network_policy_check_pending") || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("check: ok=%v problems=%+v", res.OK, res.Problems)
	for _, code := range []string{"forbidden", "cluster_unreachable", "service_missing", "network_policy_check_pending", "network_policy_check_failed"} {
		if hasProblem(res, code) {
			t.Errorf("check reports %s", code)
		}
	}
	if os.Getenv("LOOMUX_KIND_ENFORCED") == "1" {
		if hasProblem(res, "network_policy_not_enforced") {
			t.Error("a policy engine is installed; the check says not enforced")
		}
	} else if !hasProblem(res, "network_policy_not_enforced") {
		t.Error("plain kind enforces no NetworkPolicy; the check should say so")
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
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := "e2e-" + hex.EncodeToString(suffix)
	spec := protocol.EnvironmentSpec{
		ID: id, TargetID: "kind-target", Name: "kind e2e", Size: "small", Persistent: true, Egress: protocol.EgressInternet, Image: info.Image,
		SSH: protocol.SSHBootstrap{
			AuthorizedKey:  "no-port-forwarding,no-agent-forwarding,no-X11-forwarding " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(tgtSSHPub))),
			HostPrivateKey: string(pem.EncodeToMemory(hostBlock)),
			HostPublicKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSSHPub))),
			Port:           2222,
		},
		Labels: map[string]string{protocol.LabelInstance: instance, protocol.LabelTarget: "kind-target", protocol.LabelEnvironment: id},
	}
	defer func() { _ = call(protocol.MethodTargetsDestroy, protocol.IDParams{ID: id}, nil) }()
	var env protocol.Environment
	if err := call(protocol.MethodTargetsCreate, spec, &env); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s at %s:%d, %s", env.ID, env.Address.Host, env.Address.Port, env.Status)
	env = waitStatus(t, call, id, protocol.EnvRunning, 3*time.Minute)
	if env.ImageDigest == "" {
		t.Error("no image digest once running")
	}
	var health protocol.EnvironmentHealth
	if err := call(protocol.MethodTargetsHealth, protocol.IDParams{ID: id}, &health); err != nil || health.Status != protocol.EnvRunning {
		t.Errorf("health = %+v, %v", health, err)
	}

	sshIn(t, kubectl, id, tgtBlock, hostSSHPub)

	if err := call(protocol.MethodTargetsStop, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStatus(t, call, id, protocol.EnvStopped, 2*time.Minute)
	if err := call(protocol.MethodTargetsStart, protocol.IDParams{ID: id}, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, call, id, protocol.EnvRunning, 3*time.Minute)

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

	// An egress "none" machine where the cluster enforces: running only
	// once a canary on its node has said so (LOOM-208; on kind's one
	// node, the check's own canary did). TestKindNodeCanary pins one.
	if os.Getenv("LOOMUX_KIND_ENFORCED") != "1" {
		return
	}
	if !slices.Contains(info.EgressOptions, protocol.EgressNone) {
		t.Fatalf("egress none not offered where the cluster enforces: %v", info.EgressOptions)
	}
	conf := spec
	conf.ID, conf.Name, conf.Persistent, conf.Egress = "e2n-"+hex.EncodeToString(suffix), "kind e2e none", false, protocol.EgressNone
	conf.Labels = map[string]string{protocol.LabelInstance: instance, protocol.LabelTarget: "kind-target", protocol.LabelEnvironment: conf.ID}
	defer func() { _ = call(protocol.MethodTargetsDestroy, protocol.IDParams{ID: conf.ID}, nil) }()
	if err := call(protocol.MethodTargetsCreate, conf, &env); err != nil {
		t.Fatalf("create with egress none: %v", err)
	}
	waitStatus(t, call, conf.ID, protocol.EnvRunning, 3*time.Minute)
	if err := call(protocol.MethodTargetsDestroy, protocol.IDParams{ID: conf.ID}, nil); err != nil {
		t.Fatalf("destroy the none machine: %v", err)
	}
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
	var env protocol.Environment
	last := ""
	for {
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
		// The health has the cluster's last word about it.
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

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// sshIn logs in the way Loomux does: the target's key, the host key
// pinned, through a kubectl port-forward to the pod. kubectl is
// kindKubectl's flags.
func sshIn(t *testing.T, kubectl []string, id string, key *pem.Block, hostPub ssh.PublicKey) {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Log("no kubectl: skipping the ssh login")
		return
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Log("no ssh: skipping the ssh login")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pf := exec.CommandContext(ctx, "kubectl", append(kubectl, "-n", "loomux-agents", "port-forward", "pod/lx-"+id, ":2222")...)
	out, err := pf.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = pf.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	match := forwarding.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("port-forward said %q", line)
	}
	port := match[1]
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(key), 0o600); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("[127.0.0.1]:"+port+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub)))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var last []byte
	for attempt := 0; attempt < 15; attempt++ {
		cmd := exec.Command("ssh", "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile="+known,
			"-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "LogLevel=ERROR",
			"-p", port, "agent@127.0.0.1", "tmux -V && id -u && test -w /data/work && echo login-ok")
		last, err = cmd.CombinedOutput()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("ssh login through the port-forward: %v\n%s", err, last)
	}
	s := string(last)
	if !strings.Contains(s, "tmux ") || !strings.Contains(s, "\n10002\n") || !strings.Contains(s, "login-ok") {
		t.Errorf("ssh output = %q", s)
	}
	t.Logf("ssh login ok: %s", strings.Join(strings.Fields(s), " "))
	fmt.Fprintln(os.Stderr) // keep the port-forward's stderr apart from the test's
}
