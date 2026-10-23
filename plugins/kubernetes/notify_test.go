package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
)

// hostFor serves p over pipes, as a host would see it, and returns the
// targets.changed ids that host receives.
func hostFor(t *testing.T, p *Plugin) <-chan string {
	t.Helper()
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sdk.Serve(sctx, p, pluginR, pluginW)
		_ = pluginW.Close()
	}()
	ids := make(chan string, 16)
	c := rpc.NewConn(hostR, hostW, func(method string, params json.RawMessage) {
		var tc protocol.TargetsChanged
		if method == protocol.NotifyTargetsChanged && json.Unmarshal(params, &tc) == nil {
			ids <- tc.ID
		}
	})
	t.Cleanup(func() {
		c.Close()
		cancel()
		<-done
	})
	if err := c.Call(ctx, protocol.MethodDescribe, nil, nil); err != nil { // attached from here on
		t.Fatal(err)
	}
	return ids
}

// LOOM-213: a renewal that finds a machine's node open tells the host
// at once (the machine is now an error); one that confirms the node
// again changes nothing and says nothing.
func TestNodeVerdictNotifiesTheHost(t *testing.T) {
	t.Run("found open", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		ids := hostFor(t, p)
		nodes.set("n1", exitNetReachable)
		clock.advance(6 * time.Minute) // the renewal
		select {
		case id := <-ids:
			if id != "conf1" {
				t.Errorf("targets.changed for %q, want conf1", id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the host wasn't told that n1 was found open")
		}
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvError {
			t.Errorf("conf1 = %s (%s), want error", env.Status, env.Reason)
		}
	})
	t.Run("confirmed again", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		ids := hostFor(t, p)
		clock.advance(6 * time.Minute)
		if !nodes.pinnedSoon("n1", 2) {
			t.Fatalf("canaries pinned to n1 = %d, want 2: the renewal", nodes.pinnedTo("n1"))
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mu.Lock()
			recorded := p.nodes["n1"].running == nil
			p.mu.Unlock()
			if recorded {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the renewal's verdict was never recorded")
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case id := <-ids:
			t.Errorf("targets.changed for %q after a renewal that changed nothing", id)
		case <-time.After(200 * time.Millisecond):
		}
	})
}

// LOOM-223: the policies opening under a running machine, or confining
// it again, tell the host at once; a read that changes nothing says
// nothing.
func TestPolicyChangeNotifiesTheHost(t *testing.T) {
	cs, _, p, clock := runningOn(t)
	ids := hostFor(t, p)
	expect := func(what string) {
		t.Helper()
		select {
		case id := <-ids:
			if id != "conf1" {
				t.Errorf("%s: targets.changed for %q, want conf1", what, id)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the host wasn't told", what)
		}
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case id := <-ids:
			t.Errorf("%s: targets.changed for %q, want none", what, id)
		case <-time.After(200 * time.Millisecond):
		}
	}

	clock.advance(time.Minute + time.Second) // a read, nothing changed
	quiet("an unchanged read")

	nps := cs.NetworkingV1().NetworkPolicies(testNS)
	var removed []*networkingv1.NetworkPolicy
	for _, name := range []string{"agents-default-deny", "agents-allow-dns"} {
		np, err := nps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := nps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		removed = append(removed, np)
	}
	clock.advance(time.Minute + time.Second)
	expect("the policies opened")
	reasonSoon(t, p, "conf1", protocol.EnvError, "NetworkPolicies no longer confine")

	for _, np := range removed {
		np.ResourceVersion = ""
		if _, err := nps.Create(ctx, np, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(time.Minute + time.Second)
	expect("the policies confine it again")
}
