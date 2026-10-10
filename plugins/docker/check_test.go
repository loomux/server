package docker

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Loomux/server/plugins/plugintest"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
	"github.com/Loomux/server/targets/sshtest"
)

func problem(res protocol.CheckResult, code string) *protocol.Problem {
	for i := range res.Problems {
		if res.Problems[i].Code == code {
			return &res.Problems[i]
		}
	}
	return nil
}

func TestCheck(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p, _ := fakePlugin(t, nil)
		res, err := p.Check(ctx)
		if err != nil || !res.OK || len(res.Problems) != 0 {
			t.Errorf("check = %+v, %v", res, err)
		}
	})
	t.Run("image missing is a warning", func(t *testing.T) {
		p, _ := fakePlugin(t, map[string]any{"agent_image": "ghcr.io/loomux/agent:v9"})
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemImageMissing); !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning || !strings.Contains(pr.Message, "ghcr.io/loomux/agent:v9") {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("seccomp off is a warning", func(t *testing.T) {
		p, f := fakePlugin(t, nil)
		f.security = []string{"name=apparmor"}
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemSeccompDisabled); !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("engine too old", func(t *testing.T) {
		p, f := fakePlugin(t, nil)
		f.apiVersion = "1.40"
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemEngineTooOld); res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("network misconfigured", func(t *testing.T) {
		p, f := fakePlugin(t, nil)
		f.mu.Lock()
		f.networks[networkName] = &fakeNetwork{id: "n", name: networkName, driver: "bridge", options: map[string]string{"com.docker.network.bridge.enable_icc": "true"}}
		f.mu.Unlock()
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemNetworkMisconfigured); !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		p := newTestPlugin()
		if err := p.Configure(ctx, configureParams(t, map[string]any{"engine": "unix:///nonexistent/docker.sock", "bind_address": "127.0.0.1"})); err != nil {
			t.Fatal(err)
		}
		res, err := p.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if pr := problem(res, ProblemEngineUnreachable); res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Errorf("check = %+v", res)
		}
		// The problem's text never names a secret, only the engine.
		if pr := problem(res, ProblemEngineUnreachable); !strings.Contains(pr.Message, "unix:///nonexistent/docker.sock") {
			t.Errorf("message = %q", pr.Message)
		}
	})
}

// Over SSH: an unpinned host is scanned and reported, never used; a
// mismatch and a refused key are their own problems.
func TestCheckSSH(t *testing.T) {
	f := newFakeEngine()
	socket := serveUnix(t, f.handler())
	s := sshtest.Start(t)
	var calls atomic.Int32
	s.Exec = dialStdioHook(t, socket, &calls)

	configure := func(t *testing.T, pin bool, extra map[string]any) *Plugin {
		t.Helper()
		p := newTestPlugin()
		if err := p.Configure(ctx, configureParams(t, sshConfigMap(t, s, pin, extra))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Shutdown(ctx) })
		return p
	}
	t.Run("unpinned", func(t *testing.T) {
		p := configure(t, false, nil)
		res, err := p.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pr := problem(res, ProblemHostKeyUnpinned)
		if res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Fatalf("check = %+v", res)
		}
		if !strings.Contains(pr.Message, s.HostKey) || !strings.Contains(pr.Message, "SHA256:") || !strings.Contains(pr.Message, "ssh-ed25519") {
			t.Errorf("the problem should carry the key line, its type and fingerprint: %q", pr.Message)
		}
		if calls.Load() != 0 {
			t.Error("an unpinned host ran a command")
		}
		_, err = p.DescribeTargets(ctx)
		wantCode(t, err, rpc.CodeUnavailable)
		_, err = p.CreateTarget(ctx, spec("x"))
		wantCode(t, err, rpc.CodeUnavailable)
	})
	t.Run("mismatch", func(t *testing.T) {
		_, other := testKey(t)
		p := configure(t, false, map[string]any{"ssh_host_key": other})
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemHostKeyMismatch); res.OK || pr == nil || pr.Severity != protocol.SeverityError || !strings.Contains(pr.Message, "SHA256:") {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		stranger, _ := testKey(t)
		p := configure(t, true, map[string]any{"ssh_private_key": stranger})
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemUnauthorized); res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Errorf("check = %+v", res)
		}
	})
	t.Run("pinned works end to end", func(t *testing.T) {
		p := configure(t, true, nil)
		res, err := p.Check(ctx)
		if err != nil || !res.OK {
			t.Fatalf("check = %+v, %v", res, err)
		}
		env, err := p.CreateTarget(ctx, spec("overssh"))
		if err != nil || env.Status != protocol.EnvRunning {
			t.Errorf("create over ssh = %+v, %v", env, err)
		}
		if err := p.DestroyTarget(ctx, "overssh"); err != nil {
			t.Error(err)
		}
	})
}

// The plugin passes the host's conformance suite, served in-process
// over pipes against a fake engine on a unix socket.
func TestConformance(t *testing.T) {
	path := serveUnix(t, newFakeEngine().handler())
	plugintest.Run(t, func(t *testing.T) (*rpc.Conn, func()) {
		p := newTestPlugin()
		hostR, pluginW := io.Pipe()
		pluginR, hostW := io.Pipe()
		sctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = sdk.Serve(sctx, p, pluginR, pluginW)
			_ = pluginW.Close()
		}()
		conn := rpc.NewConn(hostR, hostW, nil)
		return conn, func() {
			conn.Close()
			cancel()
			<-done
			p.Shutdown(context.Background())
		}
	}, plugintest.Options{Config: map[string]any{"engine": "unix://" + path, "bind_address": "127.0.0.1", "agent_image": "ghcr.io/loomux/agent:test"}})
}
