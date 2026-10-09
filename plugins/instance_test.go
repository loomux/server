package plugins_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/registry"
)

func startFake(t *testing.T, config map[string]any, tweak func(*plugins.InstanceOptions)) (*plugins.Instance, error) {
	t.Helper()
	dir := fakeDir(t, t.TempDir())
	o := plugins.InstanceOptions{
		Label: "fake-1", Source: registry.PluginSourceBundled, Path: dir, Manifest: fakeManifest(t),
		Config: config, Host: protocol.HostInfo{Version: "test", InstanceID: "inst"},
		StopTimeout: 500 * time.Millisecond, RestartBackoff: 10 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&o)
	}
	i, err := plugins.Start(context.Background(), o)
	if err == nil {
		t.Cleanup(func() { _ = i.Stop(context.Background()) })
	}
	return i, err
}

func problems(res protocol.CheckResult) map[string]protocol.Problem {
	out := map[string]protocol.Problem{}
	for _, p := range res.Problems {
		out[p.Code] = p
	}
	return out
}

func TestInstanceStartConfigureCheck(t *testing.T) {
	t.Setenv("LOOMUX_SECRET", "must not reach the plugin")
	var calls []string
	i, err := startFake(t, map[string]any{"token": "s3cret"}, func(o *plugins.InstanceOptions) {
		o.OnCall = func(method string, err error) { calls = append(calls, method) }
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := i.Status(); s.State != plugins.StateRunning {
		t.Fatalf("status = %+v", s)
	}
	res, err := i.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	ps := problems(res)
	if leak, ok := ps[fake.ProblemEnvLeak]; ok {
		t.Errorf("the plugin saw LOOMUX_* variables: %s", leak.Message)
	}
	if _, ok := ps[fake.ProblemTokenSet]; !ok {
		t.Error("the secret didn't reach the plugin at configure")
	}
	if !res.OK {
		t.Errorf("check not ok: %+v", res)
	}
	if len(calls) != 1 || calls[0] != protocol.MethodCheck {
		t.Errorf("OnCall saw %v", calls)
	}
}

func TestInstanceRefusesManifestMismatch(t *testing.T) {
	_, err := startFake(t, nil, func(o *plugins.InstanceOptions) {
		m := *o.Manifest
		m.Version = "9.9.9"
		o.Manifest = &m
	})
	if !errors.Is(err, plugins.ErrManifestMismatch) {
		t.Fatalf("want ErrManifestMismatch, got %v", err)
	}
}

func TestInstanceMissingExecutable(t *testing.T) {
	_, err := startFake(t, nil, func(o *plugins.InstanceOptions) { o.Path = filepath.Join(o.Path, "nope") })
	if err == nil {
		t.Fatal("Start should fail without an executable")
	}
}

func TestInstanceRestartsThenFails(t *testing.T) {
	i, err := startFake(t, map[string]any{"mode": fake.ModeExitAfterConfigure}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := i.Status()
		if s.State == plugins.StateFailed {
			if s.Reason == "" {
				t.Error("a failed instance needs a reason")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %+v after exits", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	err = i.Call(context.Background(), protocol.MethodCheck, nil, nil)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeUnavailable {
		t.Errorf("call on a failed instance: %v", err)
	}
}

func TestInstanceCallTimeout(t *testing.T) {
	i, err := startFake(t, map[string]any{"mode": fake.ModeHangOnCheck}, func(o *plugins.InstanceOptions) {
		o.CallTimeout = 200 * time.Millisecond
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := i.Check(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	start := time.Now()
	if err := i.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Stop of a hung plugin took %s", time.Since(start))
	}
	if s := i.Status(); s.State != plugins.StateStopped {
		t.Errorf("status after Stop = %+v", s)
	}
}

func TestInstanceStop(t *testing.T) {
	i, err := startFake(t, nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := i.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := i.Status(); s.State != plugins.StateStopped {
		t.Errorf("status = %+v", s)
	}
	if err := i.Call(context.Background(), protocol.MethodCheck, nil, nil); !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("call after Stop: %v", err)
	}
	// Stop twice is fine.
	if err := i.Stop(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestSocketInstanceReconnects(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "fake.sock")
	stop := serveFakeSocket(t, path)
	i, err := plugins.Start(context.Background(), plugins.InstanceOptions{
		Label: "fake-sock", Source: registry.PluginSourceSocket, Path: path, Manifest: fakeManifest(t),
		ReconnectInterval: 50 * time.Millisecond, StopTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer i.Stop(context.Background())
	if _, err := i.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}

	stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := i.Call(context.Background(), protocol.MethodCheck, nil, nil)
		var rpcErr *rpc.Error
		if errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still answering after the socket went away: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := i.Status(); s.State != plugins.StateRestarting {
		t.Errorf("status while disconnected = %+v", s)
	}

	stop2 := serveFakeSocket(t, path)
	defer stop2()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := i.Check(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("didn't reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := i.Status(); s.State != plugins.StateRunning {
		t.Errorf("status after reconnect = %+v", s)
	}
}
