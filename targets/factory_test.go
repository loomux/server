package targets_test

import (
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

func TestNewExecutor(t *testing.T) {
	local, err := targets.NewExecutor(&registry.Target{Kind: registry.TargetKindLocal})
	if err != nil {
		t.Fatalf("NewExecutor(local): %v", err)
	}
	if _, ok := local.(*targets.LocalExecutor); !ok {
		t.Fatalf("NewExecutor(local) = %T, want *targets.LocalExecutor", local)
	}

	remote, err := targets.NewExecutor(&registry.Target{Kind: registry.TargetKindRemote, Host: "example.internal", User: "orski"})
	if err != nil {
		t.Fatalf("NewExecutor(remote): %v", err)
	}
	if _, ok := remote.(*targets.RemoteExecutor); !ok {
		t.Fatalf("NewExecutor(remote) = %T, want *targets.RemoteExecutor", remote)
	}

	if _, err := targets.NewExecutor(&registry.Target{Kind: "bogus"}); err == nil {
		t.Fatalf("NewExecutor(bogus kind): got nil error, want an error")
	}
}
