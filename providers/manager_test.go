package providers_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

func TestCreateMachine(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env, err := h.m.Create(ctx, providers.CreateRequest{Target: registry.Target{Name: "builds", Policy: registry.TargetPolicy{Purpose: "work"}}, PluginID: h.pluginID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if env.Status != registry.EnvironmentCreating || env.Size != "small" || !env.Persistent || env.Egress != "internet" || env.Image != "ghcr.io/loomux/agent:fake" {
		t.Errorf("environment = %+v", env)
	}
	if target.Kind != registry.TargetKindRemote || target.User != "agent" || target.SSHPort != 2222 || target.WorkspaceRoot != "/data/work" || target.SSHProxy != registry.SSHProxyNone {
		t.Errorf("target = %+v", target)
	}
	if target.Host != "lx-"+env.ID+".fake.invalid" {
		t.Errorf("host = %q", target.Host)
	}
	if !target.Managed() || target.SSHKeyRef == "" {
		t.Errorf("the target should be managed with its own key: %+v", target)
	}
	if !strings.HasPrefix(target.HostKeys, "[lx-"+env.ID+".fake.invalid]:2222 ssh-ed25519 ") {
		t.Errorf("pinned host key = %q", target.HostKeys)
	}
	if target.Policy.Purpose != "work" {
		t.Errorf("policy not carried: %+v", target.Policy)
	}
	key, err := h.store.GetSSHKey(ctx, target.SSHKeyRef)
	if err != nil || key.Origin != registry.SSHKeyOriginTarget {
		t.Errorf("the target's key = %+v, %v", key, err)
	}
	stored, _ := h.store.GetEnvironment(ctx, env.ID)
	if stored.HostKey == "" || len(stored.HostPrivateKey) == 0 || !strings.Contains(string(stored.HostPrivateKey), "PRIVATE KEY") {
		t.Error("the host key pair should be stored")
	}

	running := h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if running.ImageDigest != "sha256:fake" {
		t.Errorf("digest = %q", running.ImageDigest)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !h.probed(target.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.probed(target.ID) {
		t.Error("the target wasn't probed once the machine ran")
	}
	if !h.pluginHas(t, env.ID) {
		t.Error("the plugin doesn't have the machine")
	}
}

func TestCreateRefuses(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	cases := map[string]providers.CreateRequest{
		"size":   {Target: registry.Target{Name: "a"}, PluginID: h.pluginID, Size: "huge"},
		"egress": {Target: registry.Target{Name: "b"}, PluginID: h.pluginID, Egress: "lan"},
		"name":   {Target: registry.Target{Name: ""}, PluginID: h.pluginID},
	}
	for name, req := range cases {
		_, _, err := h.m.Create(ctx, req)
		var inv *providers.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("%s: want *InvalidError, got %v", name, err)
		}
	}
	if _, _, err := h.m.Create(ctx, providers.CreateRequest{Target: registry.Target{Name: "x"}, PluginID: "nope"}); !errors.Is(err, providers.ErrPluginUnavailable) {
		t.Errorf("unknown plugin: %v", err)
	}
	if list, _ := h.store.ListTargets(ctx); len(list) != 0 {
		t.Errorf("refused creates left targets: %+v", list)
	}
	if keys, _ := h.store.ListSSHKeys(ctx); len(keys) != 0 {
		t.Errorf("refused creates left keys: %+v", keys)
	}
}

func TestCreateRefusesBadAddress(t *testing.T) {
	h := newHarness(t, map[string]any{"address_domain": "bad;domain"})
	_, _, err := h.m.Create(context.Background(), providers.CreateRequest{Target: registry.Target{Name: "evil"}, PluginID: h.pluginID})
	var bad *providers.BadAddressError
	if !errors.As(err, &bad) {
		t.Fatalf("want *BadAddressError, got %v", err)
	}
	if list, _ := h.store.ListTargets(context.Background()); len(list) != 0 {
		t.Error("a refused address left a target row")
	}
}

func TestCreateQuotaAndPluginFailure(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		h.create(t, "m"+string(rune('a'+i)), true)
	}
	_, _, err := h.m.Create(ctx, providers.CreateRequest{Target: registry.Target{Name: "sixth"}, PluginID: h.pluginID})
	if !errors.Is(err, providers.ErrQuota) {
		t.Errorf("sixth machine: want ErrQuota, got %v", err)
	}

	failing := newHarness(t, map[string]any{"targets_mode": fake.TargetsCreateFails})
	target, _ := failing.create(t, "doomed", true)
	env := failing.waitStatus(t, target.ID, registry.EnvironmentError)
	if !strings.Contains(env.StatusReason, "create") {
		t.Errorf("reason = %q", env.StatusReason)
	}
}

func TestLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "life", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)

	h.setActive(true)
	if err := h.m.Stop(ctx, target.ID); !errors.Is(err, providers.ErrTaskActive) {
		t.Errorf("stop with a task running: %v", err)
	}
	h.setActive(false)
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e, _ := h.m.Get(ctx, target.ID); e.Status != registry.EnvironmentStopped {
		t.Errorf("after Stop = %s", e.Status)
	}
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Errorf("Stop twice: %v", err)
	}
	if err := h.m.Start(ctx, target.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)

	if err := h.m.Recreate(ctx, target.ID, "medium"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	re := h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if re.Size != "medium" {
		t.Errorf("size after recreate = %q", re.Size)
	}
	if err := h.m.Recreate(ctx, target.ID, "huge"); err == nil {
		t.Error("recreate with an unknown size should be refused")
	}

	cmds, err := h.m.AttachCommands(ctx, target.ID, "loomux-x")
	if err != nil || len(cmds) != 1 || cmds[0].Via != "fake" || !strings.Contains(cmds[0].Command, env.ID) {
		t.Errorf("AttachCommands = %+v, %v", cmds, err)
	}

	ephTarget, _ := h.create(t, "scratch", false)
	h.waitStatus(t, ephTarget.ID, registry.EnvironmentRunning)
	if err := h.m.Stop(ctx, ephTarget.ID); !errors.Is(err, providers.ErrEphemeralStop) {
		t.Errorf("stop of an ephemeral machine: %v", err)
	}

	plain := &registry.Target{ID: uuid.NewString(), Name: "laptop", Kind: registry.TargetKindRemote, Host: "laptop.example", User: "me"}
	if err := h.store.CreateTarget(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Get(ctx, plain.ID); !errors.Is(err, providers.ErrNotMachine) {
		t.Errorf("Get of a registered host: %v", err)
	}
	if err := h.m.Stop(ctx, plain.ID); !errors.Is(err, providers.ErrNotMachine) {
		t.Errorf("Stop of a registered host: %v", err)
	}
}

func TestDeleteTargetCascade(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "gone", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "proj", Path: "/data/work/proj", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateCredential(ctx, &registry.Credential{ID: uuid.NewString(), Name: "TOKEN", TargetID: target.ID, Value: "v"}); err != nil {
		t.Fatal(err)
	}
	keyID := target.SSHKeyRef

	h.setActive(true)
	if err := h.m.DeleteTarget(ctx, target.ID); !errors.Is(err, providers.ErrTaskActive) {
		t.Fatalf("delete with a task running: %v", err)
	}
	h.setActive(false)
	if err := h.m.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := h.store.GetTarget(ctx, target.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Error("target row survived")
	}
	if _, err := h.store.GetWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Error("workspace survived")
	}
	if creds, _ := h.store.ListCredentialInfo(ctx); len(creds) != 0 {
		t.Error("target-scoped credential survived")
	}
	if _, err := h.store.GetSSHKey(ctx, keyID); !errors.Is(err, registry.ErrNotFound) {
		t.Error("the generated key survived")
	}
	if h.pluginHas(t, env.ID) {
		t.Error("the plugin still has the machine")
	}
	if err := h.m.DeleteTarget(ctx, target.ID); !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, providers.ErrNotMachine) {
		t.Errorf("DeleteTarget twice: %v", err)
	}
}

func TestEnsureRunning(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, _ := h.create(t, "sleepy", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.m.EnsureRunning(ctx, target.ID); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if e, _ := h.m.Get(ctx, target.ID); e.Status != registry.EnvironmentRunning {
		t.Errorf("after EnsureRunning = %s", e.Status)
	}
	if err := h.m.EnsureRunning(ctx, target.ID); err != nil {
		t.Errorf("EnsureRunning on a running machine: %v", err)
	}
}

func TestReconcileLostAndRecreated(t *testing.T) {
	h := newHarness(t, map[string]any{"targets_mode": fake.TargetsVanish})
	ctx := context.Background()
	eph, _ := h.create(t, "eph", false)
	per, _ := h.create(t, "per", true)
	h.waitStatus(t, eph.ID, registry.EnvironmentRunning)
	h.waitStatus(t, per.ID, registry.EnvironmentRunning)
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "on-eph", Path: "/data/work/x", TargetID: eph.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the fake makes running machines vanish after a second
	h.m.Reconcile(ctx)
	if e, _ := h.m.Get(ctx, eph.ID); e.Status != registry.EnvironmentLost {
		t.Errorf("ephemeral after vanishing = %s (%s)", e.Status, e.StatusReason)
	}
	if w, _ := h.store.GetWorkspace(ctx, ws.ID); w.Status != registry.WorkspaceStatusArchived || !strings.Contains(w.StatusReason, "lost") {
		t.Errorf("workspace on the lost machine = %+v", w)
	}
	// The persistent one is made again and comes back.
	deadline := time.Now().Add(10 * time.Second)
	for {
		e, _ := h.m.Get(ctx, per.ID)
		if e.Status == registry.EnvironmentRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persistent machine never came back: %s (%s)", e.Status, e.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.pluginHas(t, perEnvID(t, h, per.ID)) {
		t.Error("the plugin doesn't have the recreated machine")
	}
}

func perEnvID(t *testing.T, h *harness, targetID string) string {
	t.Helper()
	e, err := h.m.Get(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// Review focus 2: a row left creating by a crash is resumed, and the
// machine comes up once.
func TestReconcileResumesCreating(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	key, err := targets.GenerateSSHKey(uuid.NewString(), "resumed")
	if err != nil {
		t.Fatal(err)
	}
	key.Origin = registry.SSHKeyOriginTarget
	if err := h.store.CreateSSHKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "test")
	sshPub, _ := ssh.NewPublicKey(pub)
	target := &registry.Target{ID: uuid.NewString(), Name: "resumed", Kind: registry.TargetKindRemote, Host: "lx-resume01.fake.invalid", User: "agent", SSHPort: 2222, SSHKeyRef: key.ID, SSHProxy: registry.SSHProxyNone, WorkspaceRoot: "/data/work"}
	if err := h.store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	env := &registry.Environment{ID: "resume01", TargetID: target.ID, PluginID: h.pluginID, PluginName: "fake", PluginVersion: "0.1.0",
		Status: registry.EnvironmentCreating, Size: "small", Persistent: true, Egress: "internet", Image: "ghcr.io/loomux/agent:fake",
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), HostPrivateKey: pem.EncodeToMemory(block)}
	if err := h.store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	if h.pluginHas(t, env.ID) {
		t.Fatal("the plugin shouldn't have it yet")
	}
	h.m.Reconcile(ctx)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if !h.pluginHas(t, env.ID) {
		t.Error("the plugin doesn't have the resumed machine")
	}
	// A second reconcile changes nothing.
	h.m.Reconcile(ctx)
	if e, _ := h.m.Get(ctx, target.ID); e.Status != registry.EnvironmentRunning {
		t.Errorf("after a second reconcile = %s", e.Status)
	}
}

func TestReconcileStopsWhatShouldBeStopped(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "stopme", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	// Someone starts it behind Loomux's back.
	if err := h.pm.Call(ctx, h.pluginID, protocol.MethodTargetsStart, protocol.IDParams{ID: env.ID}, nil); err != nil {
		t.Fatal(err)
	}
	h.m.Reconcile(ctx)
	var pe protocol.Environment
	_ = h.pm.Call(ctx, h.pluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: env.ID}, &pe)
	if pe.Status != protocol.EnvStopped {
		t.Errorf("the plugin's machine after reconcile = %s, want stopped", pe.Status)
	}
}

func TestOrphanSweep(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	spec := protocol.EnvironmentSpec{ID: "orphan01", Name: "orphan", Size: "small", Persistent: true, Egress: "internet", Image: "x",
		SSH: protocol.SSHBootstrap{Port: 2222}, Labels: map[string]string{protocol.LabelInstance: "inst"}}
	if err := h.pm.Call(ctx, h.pluginID, protocol.MethodTargetsCreate, spec, nil); err != nil {
		t.Fatal(err)
	}
	other := spec
	other.ID, other.Labels = "foreign01", map[string]string{protocol.LabelInstance: "another-loomux"}
	if err := h.pm.Call(ctx, h.pluginID, protocol.MethodTargetsCreate, other, nil); err != nil {
		t.Fatal(err)
	}
	h.m.OrphanTTL = time.Hour
	h.m.Reconcile(ctx)
	if !h.pluginHas(t, "orphan01") {
		t.Fatal("a young orphan must be left alone")
	}
	h.m.OrphanTTL = 0
	h.m.Reconcile(ctx)
	if h.pluginHas(t, "orphan01") {
		t.Error("an old orphan should be destroyed")
	}
	// The fake's list is filtered by instance, so another instance's
	// machine is never seen, let alone destroyed.
	var list protocol.ListResult
	_ = h.pm.Call(ctx, h.pluginID, protocol.MethodTargetsList, nil, &list)
	for _, e := range list.Environments {
		if e.ID == "foreign01" {
			t.Error("another instance's machine was listed as ours")
		}
	}
}

func TestUninstallKeepAndDestroy(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "kept", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if n, _ := h.m.MachineCount(ctx, h.pluginID); n != 1 {
		t.Fatalf("MachineCount = %d", n)
	}
	if err := h.pm.Uninstall(ctx, h.pluginID, ""); err == nil {
		t.Fatal("uninstall with a machine and no choice should be refused")
	}
	if err := h.pm.Uninstall(ctx, h.pluginID, plugins.UninstallKeep); err != nil {
		t.Fatalf("uninstall keep: %v", err)
	}
	e, _ := h.m.Get(ctx, target.ID)
	if e.Status != registry.EnvironmentDetached || e.PluginID != "" || e.PluginName != "fake" {
		t.Errorf("after uninstall keep = %+v", e)
	}
	if _, err := h.store.GetTarget(ctx, target.ID); err != nil {
		t.Error("the target should stay as a plain host")
	}
	if err := h.m.Stop(ctx, target.ID); !errors.Is(err, providers.ErrDetached) {
		t.Errorf("stop of a detached machine: %v", err)
	}
	if err := h.m.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatalf("delete of a detached machine: %v", err)
	}
	_ = env

	// Destroy mode on a fresh install.
	h2 := newHarness(t, nil)
	t2, e2 := h2.create(t, "doomed", true)
	h2.waitStatus(t, t2.ID, registry.EnvironmentRunning)
	if err := h2.pm.Uninstall(ctx, h2.pluginID, plugins.UninstallDestroy); err != nil {
		t.Fatalf("uninstall destroy: %v", err)
	}
	if _, err := h2.store.GetTarget(ctx, t2.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Error("the target should be gone")
	}
	if envs, _ := h2.store.ListEnvironments(ctx); len(envs) != 0 {
		t.Errorf("environments after uninstall destroy = %+v", envs)
	}
	_ = e2
}
