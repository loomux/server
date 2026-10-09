// Package plugintest is the conformance suite a loomux-plugin/1 plugin
// must pass (docs/design/target-providers.md §12): the analogue of
// registry/storetest and targets/executortest. Run drives a plugin
// through the plugin.* group as the host would and checks every answer
// is one the host can take.
package plugintest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// Options tune the suite for one plugin.
type Options struct {
	// Config is a valid configuration for the plugin (its required
	// settings); nil for a plugin with none. Secrets may be test values.
	Config map[string]any
	// CallTimeout bounds each call; 30 s by default, the host's own.
	CallTimeout time.Duration
}

// Launch connects to a fresh instance of the plugin under test (a
// subprocess, a socket, or an in-process Serve over pipes) and returns
// the host's connection and how to stop it.
type Launch func(t *testing.T) (*rpc.Conn, func())

// Run runs the suite. Each case gets its own launch.
func Run(t *testing.T, launch Launch, o Options) {
	if o.CallTimeout == 0 {
		o.CallTimeout = 30 * time.Second
	}
	t.Run("DescribeIsAValidManifest", func(t *testing.T) { testDescribe(t, launch, o) })
	t.Run("ConfigureAndCheck", func(t *testing.T) { testConfigureAndCheck(t, launch, o) })
	t.Run("ConfigureRefusesBadParams", func(t *testing.T) { testBadParams(t, launch, o) })
	t.Run("UnknownMethod", func(t *testing.T) { testUnknownMethod(t, launch, o) })
	t.Run("UndeclaredCapabilityRefused", func(t *testing.T) { testUndeclared(t, launch, o) })
	t.Run("LargeParamsDontHang", func(t *testing.T) { testLargeParams(t, launch, o) })
	t.Run("Shutdown", func(t *testing.T) { testShutdown(t, launch, o) })
	RunTargets(t, launch, o)
}

func call(t *testing.T, conn *rpc.Conn, o Options, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), o.CallTimeout)
	defer cancel()
	return conn.Call(ctx, method, params, result)
}

func describe(t *testing.T, conn *rpc.Conn, o Options) *plugins.Manifest {
	t.Helper()
	var m plugins.Manifest
	if err := call(t, conn, o, protocol.MethodDescribe, nil, &m); err != nil {
		t.Fatalf("plugin.describe: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("plugin.describe returned a manifest the host refuses: %v", err)
	}
	return &m
}

func configure(t *testing.T, conn *rpc.Conn, o Options, m *plugins.Manifest) {
	t.Helper()
	schema, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	cfg := schema.ApplyDefaults(o.Config)
	if err := schema.Validate(cfg); err != nil {
		t.Fatalf("Options.Config isn't valid for this plugin's schema: %v", err)
	}
	params := protocol.ConfigureParams{Config: cfg, Host: protocol.HostInfo{Version: "plugintest", InstanceID: "plugintest-instance"}}
	if err := call(t, conn, o, protocol.MethodConfigure, params, nil); err != nil {
		t.Fatalf("plugin.configure: %v", err)
	}
}

func testDescribe(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	first := describe(t, conn, o)
	second := describe(t, conn, o)
	if !first.Equal(second) {
		t.Errorf("plugin.describe isn't stable: %+v then %+v", first, second)
	}
	if first.Title == "" || first.Description == "" {
		t.Errorf("the manifest should carry a title and a description for the catalog; got %q / %q", first.Title, first.Description)
	}
}

func testConfigureAndCheck(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	configure(t, conn, o, m)
	// Configure twice: the host does that after a configuration change.
	configure(t, conn, o, m)
	var res protocol.CheckResult
	if err := call(t, conn, o, protocol.MethodCheck, nil, &res); err != nil {
		t.Fatalf("plugin.check: %v", err)
	}
	if res.Problems == nil {
		t.Error("plugin.check must return problems as a list, not null")
	}
	hasError := false
	for _, p := range res.Problems {
		if p.Code == "" || p.Message == "" {
			t.Errorf("a problem needs a code and a message: %+v", p)
		}
		if strings.ToLower(p.Code) != p.Code || strings.ContainsAny(p.Code, " \t") {
			t.Errorf("a problem code is a stable lowercase identifier: %q", p.Code)
		}
		switch p.Severity {
		case protocol.SeverityWarning:
		case protocol.SeverityError:
			hasError = true
		default:
			t.Errorf("a problem's severity is warning or error: %q", p.Severity)
		}
	}
	if hasError && res.OK {
		t.Error("plugin.check says ok with an error-severity problem")
	}
}

func testBadParams(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	describe(t, conn, o)
	err := call(t, conn, o, protocol.MethodConfigure, "not an object", nil)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeInvalidParams {
		t.Errorf("plugin.configure with a string: want invalid_params, got %v", err)
	}
	// The connection is still usable afterwards.
	describe(t, conn, o)
}

func testUnknownMethod(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	err := call(t, conn, o, "plugintest.nothing", nil, nil)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeMethodNotFound {
		t.Errorf("unknown method: want method_not_found, got %v", err)
	}
	describe(t, conn, o)
}

func testUndeclared(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	if m.HasCapability("targets.create") {
		t.Skip("the plugin declares targets.*; the targets suite covers it")
	}
	configure(t, conn, o, m)
	err := call(t, conn, o, "targets.list", nil, nil)
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeMethodNotFound {
		t.Errorf("a group the manifest doesn't declare: want method_not_found, got %v", err)
	}
}

func testLargeParams(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	m := describe(t, conn, o)
	configure(t, conn, o, m)
	// Half a MiB of parameters is within the frame limit; the plugin
	// must answer (ok, or an error it chooses), never stall.
	big := map[string]any{"config": map[string]any{"plugintest_padding": strings.Repeat("x", 512<<10)}, "host": protocol.HostInfo{Version: "plugintest"}}
	ctx, cancel := context.WithTimeout(context.Background(), o.CallTimeout)
	defer cancel()
	err := conn.Call(ctx, protocol.MethodConfigure, big, nil)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, rpc.ErrClosed) {
		t.Fatalf("a large configure stalled or broke the connection: %v", err)
	}
	// Whatever it answered, it still answers.
	configure(t, conn, o, m)
}

func testShutdown(t *testing.T, launch Launch, o Options) {
	conn, stop := launch(t)
	defer stop()
	describe(t, conn, o)
	if err := call(t, conn, o, protocol.MethodShutdown, nil, nil); err != nil {
		t.Errorf("plugin.shutdown: %v", err)
	}
}
