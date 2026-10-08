package targets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-141: a local target's processes don't inherit loomuxd's secrets.
func TestLocalEnv_KeepsOnlyAllowedVariables(t *testing.T) {
	t.Setenv("LOOMUX_MASTER_KEY", "vault-key")
	t.Setenv("LOOMUX_ROUTER_PRIMARY_API_KEY", "router-key")
	t.Setenv("LC_ALL", "C.UTF-8")
	env := strings.Join(localEnv(), "\n")
	for _, secret := range []string{"vault-key", "router-key", "LOOMUX_"} {
		if strings.Contains(env, secret) {
			t.Errorf("local env carries %q:\n%s", secret, env)
		}
	}
	for _, kept := range []string{"PATH=", "HOME=", "LC_ALL=C.UTF-8"} {
		if !strings.Contains(env, kept) {
			t.Errorf("local env lacks %s", kept)
		}
	}
}

func TestLocalExecutor_RunOnceDoesNotSeeLoomuxSecrets(t *testing.T) {
	t.Setenv("LOOMUX_MASTER_KEY", "vault-key")
	out, err := NewLocalExecutor().RunOnce(context.Background(), "env")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if strings.Contains(out, "vault-key") || strings.Contains(out, "LOOMUX_MASTER_KEY") {
		t.Fatalf("a local command saw loomuxd's vault key:\n%s", out)
	}
}

// LOOM-141: with local targets off, nothing runs on one.
func TestNewExecutor_LocalTargetsOff(t *testing.T) {
	defer SetLocalTargets(LocalTargets)
	SetLocalTargets(false)
	_, err := NewExecutor(&registry.Target{Name: "box", Kind: registry.TargetKindLocal})
	if !errors.Is(err, ErrLocalTargetsOff) {
		t.Fatalf("NewExecutor(local) err = %v, want ErrLocalTargetsOff", err)
	}
	if _, err := NewExecutor(&registry.Target{Name: "r", Kind: registry.TargetKindRemote, Host: "h"}); err != nil {
		t.Fatalf("NewExecutor(remote) with local off: %v", err)
	}
}
