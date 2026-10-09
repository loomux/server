// Package fake is a plugin for tests (docs/design/target-providers.md
// §12): no infrastructure behind it, and a configuration that scripts
// how it behaves, so the host's supervisor, manager and API can be
// exercised against a real subprocess. cmd/loomux-plugin-fake is its
// executable.
package fake

import (
	"context"
	_ "embed"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
)

// ManifestJSON is the fake's plugin.json, the one its executable serves
// at describe; tests write it beside a built binary.
//
//go:embed plugin.json
var ManifestJSON []byte

// Modes the "mode" setting selects.
const (
	ModeOK                 = "ok"
	ModeCrashOnCheck       = "crash_on_check"
	ModeHangOnCheck        = "hang_on_check"
	ModeExitAfterConfigure = "exit_after_configure"
)

// Problem codes the fake reports.
const (
	// ProblemEnvLeak (error) lists every LOOMUX_* variable in the
	// plugin's environment: there must be none.
	ProblemEnvLeak = "env_leak"
	// ProblemTokenSet (warning) says the secret "token" arrived, without
	// repeating it.
	ProblemTokenSet = "token_set"
)

// Plugin is the fake. Exit is what a crash or an exit calls; os.Exit by
// default, replaceable by tests that serve the fake in-process.
type Plugin struct {
	Exit func(code int)

	mu       sync.Mutex
	mode     string
	token    string
	greeting string
	host     protocol.HostInfo
	shutdown bool
}

// New returns a fake in mode ok.
func New() *Plugin {
	return &Plugin{Exit: os.Exit, mode: ModeOK, greeting: "hi"}
}

func (p *Plugin) Describe(ctx context.Context) (*plugins.Manifest, error) {
	return plugins.ParseManifest(ManifestJSON)
}

func (p *Plugin) Configure(ctx context.Context, cp protocol.ConfigureParams) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := cp.Config["mode"].(string); ok && m != "" {
		p.mode = m
	}
	p.token, _ = cp.Config["token"].(string)
	if g, ok := cp.Config["greeting"].(string); ok {
		p.greeting = g
	}
	p.host = cp.Host
	if p.mode == ModeExitAfterConfigure {
		// Answer first, then go: the host sees a configured plugin die.
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Exit(0)
		}()
	}
	return nil
}

func (p *Plugin) Check(ctx context.Context) (protocol.CheckResult, error) {
	p.mu.Lock()
	mode, token := p.mode, p.token
	p.mu.Unlock()
	switch mode {
	case ModeCrashOnCheck:
		p.Exit(3)
	case ModeHangOnCheck:
		<-ctx.Done()
		return protocol.CheckResult{}, ctx.Err()
	}
	res := protocol.CheckResult{OK: true, Problems: []protocol.Problem{}}
	if leaked := loomuxEnv(); len(leaked) > 0 {
		res.OK = false
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemEnvLeak, Severity: protocol.SeverityError,
			Message: "the host leaked its environment: " + strings.Join(leaked, ", "),
		})
	}
	if token != "" {
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemTokenSet, Severity: protocol.SeverityWarning,
			Message: "a token is configured",
		})
	}
	return res, nil
}

func (p *Plugin) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.shutdown = true
	p.mu.Unlock()
	return nil
}

// Host is what Configure last received about the host.
func (p *Plugin) Host() protocol.HostInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// ShutdownCalled reports whether the host asked the fake to shut down.
func (p *Plugin) ShutdownCalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shutdown
}

// loomuxEnv lists the LOOMUX_* variable names in the environment,
// sorted; values are never included.
func loomuxEnv() []string {
	var names []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "LOOMUX_") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
