package sdk_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
)

// serve runs the fake over pipes and returns the host's conn.
func serve(t *testing.T, p sdk.Plugin) *rpc.Conn {
	t.Helper()
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sdk.Serve(ctx, p, pluginR, pluginW)
		_ = pluginW.Close()
	}()
	c := rpc.NewConn(hostR, hostW, nil)
	t.Cleanup(func() {
		c.Close()
		cancel()
		<-done
	})
	return c
}

func TestServePluginMethods(t *testing.T) {
	t.Setenv("LOOMUX_TEST_LEAK", "x")
	p := fake.New()
	c := serve(t, p)
	ctx := context.Background()

	var m plugins.Manifest
	if err := c.Call(ctx, protocol.MethodDescribe, nil, &m); err != nil {
		t.Fatalf("describe: %v", err)
	}
	want, _ := plugins.ParseManifest(fake.ManifestJSON)
	if !m.Equal(want) {
		t.Errorf("describe = %+v", m)
	}

	err := c.Call(ctx, protocol.MethodConfigure, protocol.ConfigureParams{
		Config: map[string]any{"token": "s3cret", "greeting": "yo"},
		Host:   protocol.HostInfo{Version: "test", InstanceID: "inst"},
	}, nil)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if p.Host().InstanceID != "inst" {
		t.Errorf("host info not delivered: %+v", p.Host())
	}

	var res protocol.CheckResult
	if err := c.Call(ctx, protocol.MethodCheck, nil, &res); err != nil {
		t.Fatalf("check: %v", err)
	}
	codes := map[string]protocol.Problem{}
	for _, pr := range res.Problems {
		codes[pr.Code] = pr
	}
	if res.OK {
		t.Error("check should fail: the test's own env has LOOMUX_TEST_LEAK")
	}
	if leak, ok := codes[fake.ProblemEnvLeak]; !ok || leak.Severity != protocol.SeverityError || leak.Message == "" {
		t.Errorf("env_leak problem = %+v", codes)
	}
	if tok, ok := codes[fake.ProblemTokenSet]; !ok || tok.Severity != protocol.SeverityWarning {
		t.Errorf("token_set problem = %+v", codes)
	}
	for _, pr := range res.Problems {
		if contains(pr.Message, "s3cret") {
			t.Error("a problem message repeated the secret")
		}
	}

	err = c.Call(ctx, "notify.send", map[string]any{}, nil)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeMethodNotFound {
		t.Errorf("undeclared group: %v", err)
	}
	// The fake implements TargetProvider, so targets.* is served.
	var info protocol.TargetsInfo
	if err := c.Call(ctx, protocol.MethodTargetsDescribe, nil, &info); err != nil || info.Port != 2222 {
		t.Errorf("targets.describe: %+v, %v", info, err)
	}
	err = c.Call(ctx, protocol.MethodConfigure, "not an object", nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeInvalidParams {
		t.Errorf("bad params: %v", err)
	}

	if err := c.Call(ctx, protocol.MethodShutdown, nil, nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !p.ShutdownCalled() {
		t.Error("Shutdown not delivered")
	}
}

func TestHangRespectsContext(t *testing.T) {
	p := fake.New()
	c := serve(t, p)
	if err := c.Call(context.Background(), protocol.MethodConfigure, protocol.ConfigureParams{Config: map[string]any{"mode": fake.ModeHangOnCheck}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, protocol.MethodCheck, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestCrashCallsExit(t *testing.T) {
	p := fake.New()
	exited := make(chan int, 1)
	// In-process the stub returns (a real os.Exit never does), so the
	// handler finishes and the server can stop.
	p.Exit = func(code int) { exited <- code }
	c := serve(t, p)
	if err := c.Call(context.Background(), protocol.MethodConfigure, protocol.ConfigureParams{Config: map[string]any{"mode": fake.ModeCrashOnCheck}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Call(ctx, protocol.MethodCheck, nil, nil)
	select {
	case code := <-exited:
		if code != 3 {
			t.Errorf("exit code = %d", code)
		}
	default:
		t.Fatal("Exit not called")
	}
}

// TestSocketServe runs the fake's socket form in-process (the sidecar
// shape) and talks to it over the socket.
func TestSocketServe(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "fake.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- sdk.RunForTest(ctx, fake.New(), []string{"--listen", path}) }()
	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, %v", fi.Mode(), err)
	}
	c := rpc.NewConn(conn, conn, nil)
	var m plugins.Manifest
	if err := c.Call(context.Background(), protocol.MethodDescribe, nil, &m); err != nil || m.Name != "fake" {
		t.Fatalf("describe over socket: %+v, %v", m, err)
	}
	c.Close()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("socket server didn't stop")
	}
}

func TestBadFlag(t *testing.T) {
	if code := sdk.RunForTest(context.Background(), fake.New(), []string{"--nope"}); code != 2 {
		t.Errorf("bad flag exit = %d", code)
	}
}

// shortTempDir is a temp dir whose path fits a unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lxsdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
