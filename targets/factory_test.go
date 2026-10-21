package targets_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Loomux/server/internal/metrics"
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

// LOOM-202: an executor NewExecutorWith builds is on the options' tmux
// socket, local or remote, wrapped for metrics or not; no socket is
// DefaultTmuxSocket, and a bad one is refused.
func TestNewExecutorWith_TmuxSocket(t *testing.T) {
	local := &registry.Target{Kind: registry.TargetKindLocal}
	remote := &registry.Target{Kind: registry.TargetKindRemote, Host: "example.internal", User: "orski"}
	met := metrics.NewMetrics(prometheus.NewRegistry())
	for _, tgt := range []*registry.Target{local, remote} {
		for _, m := range []*metrics.Metrics{nil, met} {
			for socket, want := range map[string]string{"loomux-test": "loomux-test", "": targets.DefaultTmuxSocket} {
				e, err := targets.NewExecutorWith(tgt, targets.ExecutorOptions{TmuxSocket: socket, Metrics: m})
				if err != nil {
					t.Fatalf("%s, metrics %v, socket %q: %v", tgt.Kind, m != nil, socket, err)
				}
				if got := e.TmuxSocket(); got != want {
					t.Errorf("%s, metrics %v, socket %q: TmuxSocket() = %q, want %q", tgt.Kind, m != nil, socket, got, want)
				}
			}
			if _, err := targets.NewExecutorWith(tgt, targets.ExecutorOptions{TmuxSocket: "-S /tmp/x", Metrics: m}); err == nil {
				t.Errorf("%s: a bad socket name was accepted", tgt.Kind)
			}
		}
	}
}
