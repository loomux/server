package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/Loomux/server/registry"
)

// Environments (LOOM-178).

func newEnvironment(targetID, pluginID string) *registry.Environment {
	return &registry.Environment{
		ID: "k7f3q2" + targetID[len(targetID)-1:], TargetID: targetID, PluginID: pluginID,
		PluginName: "fake", PluginVersion: "0.1.0", Status: registry.EnvironmentCreating,
		Size: "small", Persistent: true, Egress: "internet", Image: "ghcr.io/loomux/agent:fake",
		HostKey: "ssh-ed25519 AAAA host", HostPrivateKey: []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"),
	}
}

func testEnvironmentCRUD(t *testing.T, store registry.Store) {
	ctx := context.Background()
	target := &registry.Target{ID: "t-env-1", Name: "machine-1", Kind: registry.TargetKindRemote, Host: "lx-1.fake.invalid", User: "agent"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	plugin := newPlugin("env-owner")
	plugin.Secrets = nil
	if err := store.CreatePlugin(ctx, plugin); err != nil {
		t.Fatal(err)
	}
	e := newEnvironment(target.ID, plugin.ID)
	if err := store.CreateEnvironment(ctx, e); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if e.CreatedAt.IsZero() {
		t.Error("CreateEnvironment should stamp CreatedAt")
	}

	got, err := store.GetEnvironment(ctx, e.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.TargetID != target.ID || got.PluginID != plugin.ID || got.Status != registry.EnvironmentCreating || string(got.HostPrivateKey) != string(e.HostPrivateKey) {
		t.Errorf("GetEnvironment = %+v", got)
	}
	byTarget, err := store.GetEnvironmentByTarget(ctx, target.ID)
	if err != nil || byTarget.ID != e.ID {
		t.Fatalf("GetEnvironmentByTarget = %+v, %v", byTarget, err)
	}
	if _, err := store.GetEnvironmentByTarget(ctx, "no-such-target"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetEnvironmentByTarget unknown: %v", err)
	}

	list, err := store.ListEnvironments(ctx)
	if err != nil || len(list) != 1 || list[0].HostPrivateKey != nil {
		t.Fatalf("ListEnvironments = %+v, %v (keys must not be decrypted)", list, err)
	}
	byPlugin, err := store.ListEnvironmentsByPlugin(ctx, plugin.ID)
	if err != nil || len(byPlugin) != 1 {
		t.Fatalf("ListEnvironmentsByPlugin = %+v, %v", byPlugin, err)
	}
	if other, _ := store.ListEnvironmentsByPlugin(ctx, "nobody"); len(other) != 0 {
		t.Errorf("ListEnvironmentsByPlugin for another plugin = %+v", other)
	}

	// A second environment for the same target is a conflict; one for an
	// unknown target or plugin too.
	dup := newEnvironment(target.ID, plugin.ID)
	dup.ID = "dup"
	if err := store.CreateEnvironment(ctx, dup); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("second environment for a target: want ErrConflict, got %v", err)
	}
	bad := newEnvironment("no-target", plugin.ID)
	bad.ID = "bad"
	if err := store.CreateEnvironment(ctx, bad); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("environment for an unknown target: want ErrConflict, got %v", err)
	}

	got.Status, got.StatusReason, got.ImageDigest, got.PluginID = registry.EnvironmentRunning, "", "sha256:abc", ""
	if err := store.UpdateEnvironment(ctx, got); err != nil {
		t.Fatalf("UpdateEnvironment: %v", err)
	}
	again, _ := store.GetEnvironment(ctx, e.ID)
	if again.Status != registry.EnvironmentRunning || again.ImageDigest != "sha256:abc" || again.PluginID != "" || again.PluginName != "fake" {
		t.Errorf("after update = %+v", again)
	}
	if string(again.HostPrivateKey) != string(e.HostPrivateKey) {
		t.Error("UpdateEnvironment must not touch the host key")
	}

	// The target can't go while its environment exists; the plugin
	// could now (plugin_id cleared), but not before.
	if err := store.DeleteTarget(ctx, target.ID); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("DeleteTarget with an environment: want ErrConflict, got %v", err)
	}
	if err := store.DeleteEnvironment(ctx, e.ID); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if err := store.DeleteEnvironment(ctx, e.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("DeleteEnvironment twice: want ErrNotFound, got %v", err)
	}
	if err := store.DeleteTarget(ctx, target.ID); err != nil {
		t.Errorf("DeleteTarget after the environment is gone: %v", err)
	}
}

func testEnvironmentPluginRestrict(t *testing.T, store registry.Store) {
	ctx := context.Background()
	target := &registry.Target{ID: "t-env-2", Name: "machine-2", Kind: registry.TargetKindRemote, Host: "lx-2.fake.invalid", User: "agent"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	plugin := newPlugin("env-owner-2")
	plugin.Secrets = nil
	if err := store.CreatePlugin(ctx, plugin); err != nil {
		t.Fatal(err)
	}
	e := newEnvironment(target.ID, plugin.ID)
	if err := store.CreateEnvironment(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePlugin(ctx, plugin.ID); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("DeletePlugin with environments: want ErrConflict, got %v", err)
	}
	e.PluginID = ""
	if err := store.UpdateEnvironment(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePlugin(ctx, plugin.ID); err != nil {
		t.Errorf("DeletePlugin after detaching: %v", err)
	}
}

// testEnvironmentNeedsMasterKey runs on a store without a master key.
func testEnvironmentNeedsMasterKey(t *testing.T, store registry.Store) {
	ctx := context.Background()
	target := &registry.Target{ID: "t-env-3", Name: "machine-3", Kind: registry.TargetKindRemote, Host: "lx-3.fake.invalid", User: "agent"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEnvironment(ctx, newEnvironment(target.ID, "")); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Errorf("CreateEnvironment without a key: want ErrNoMasterKey, got %v", err)
	}
}

// A credential scoped to a target (LOOM-178): stored, listed, unique per
// scope, and the target can't go while it exists.
func testCredentialTargetScope(t *testing.T, store registry.Store) {
	ctx := context.Background()
	target := &registry.Target{ID: "t-cred-1", Name: "machine-cred", Kind: registry.TargetKindRemote, Host: "lx-c.fake.invalid", User: "agent"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	c := &registry.Credential{ID: "cred-target-1", Name: "ANTHROPIC_API_KEY", TargetID: target.ID, Value: "sk-target"}
	if err := store.CreateCredential(ctx, c); err != nil {
		t.Fatalf("CreateCredential with a target scope: %v", err)
	}
	global := &registry.Credential{ID: "cred-global-1", Name: "ANTHROPIC_API_KEY", Value: "sk-global"}
	if err := store.CreateCredential(ctx, global); err != nil {
		t.Fatalf("the same name unscoped is another scope: %v", err)
	}
	dup := &registry.Credential{ID: "cred-target-2", Name: "ANTHROPIC_API_KEY", TargetID: target.ID, Value: "again"}
	if err := store.CreateCredential(ctx, dup); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("same name and target: want ErrConflict, got %v", err)
	}
	bad := &registry.Credential{ID: "cred-target-3", Name: "X", TargetID: "no-such-target", Value: "v"}
	if err := store.CreateCredential(ctx, bad); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("unknown target: want ErrConflict, got %v", err)
	}
	got, err := store.GetCredential(ctx, c.ID)
	if err != nil || got.TargetID != target.ID || got.Value != "sk-target" {
		t.Fatalf("GetCredential = %+v, %v", got, err)
	}
	info, err := store.ListCredentialInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range info {
		if i.ID == c.ID && i.TargetID == target.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("ListCredentialInfo lacks the target scope: %+v", info)
	}
	if err := store.DeleteTarget(ctx, target.ID); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("DeleteTarget with a scoped credential: want ErrConflict, got %v", err)
	}
	if err := store.DeleteCredential(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTarget(ctx, target.ID); err != nil {
		t.Errorf("DeleteTarget after the credential is gone: %v", err)
	}
}
