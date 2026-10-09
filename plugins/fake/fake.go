// Package fake is a plugin for tests (docs/design/target-providers.md
// §12): no infrastructure behind it, and a configuration that scripts
// how it behaves, so the host's supervisor, manager and API can be
// exercised against a real subprocess. cmd/loomux-plugin-fake is its
// executable.
package fake

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// ModeLeakToken makes the plugin misbehave the way the host must
	// guard against: its check's message and its stderr carry the
	// configured token (and the one before it, after a change).
	ModeLeakToken = "leak_token"
	// ModeRejectConfigure makes Configure fail with an error that
	// repeats the token: a plugin that validates badly.
	ModeRejectConfigure = "reject_configure"
)

// Problem codes the fake reports.
const (
	// ProblemEnvLeak (error) lists every LOOMUX_* variable in the
	// plugin's environment: there must be none.
	ProblemEnvLeak = "env_leak"
	// ProblemTokenSet (warning) says the secret "token" arrived, without
	// repeating it.
	ProblemTokenSet = "token_set"
	// ProblemTokenLeaked (warning, mode leak_token) repeats the token.
	ProblemTokenLeaked = "token_leaked"
)

// Plugin is the fake. Exit is what a crash or an exit calls; os.Exit by
// default, replaceable by tests that serve the fake in-process. Stderr
// is where mode leak_token writes; os.Stderr by default.
type Plugin struct {
	Exit   func(code int)
	Stderr func(line string)

	mu        sync.Mutex
	mode      string
	token     string
	prevToken string
	greeting  string
	host      protocol.HostInfo
	shutdown  bool
}

// New returns a fake in mode ok.
func New() *Plugin {
	return &Plugin{
		Exit:     os.Exit,
		Stderr:   func(line string) { fmt.Fprintln(os.Stderr, line) },
		mode:     ModeOK,
		greeting: "hi",
	}
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
	if t, _ := cp.Config["token"].(string); t != p.token {
		p.prevToken, p.token = p.token, t
	}
	if g, ok := cp.Config["greeting"].(string); ok {
		p.greeting = g
	}
	p.host = cp.Host
	if dir, ok := cp.Config["pid_dir"].(string); ok && dir != "" {
		_ = os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), []byte(p.host.InstanceID), 0o644)
	}
	switch p.mode {
	case ModeExitAfterConfigure:
		// Answer first, then go: the host sees a configured plugin die.
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Exit(0)
		}()
	case ModeLeakToken:
		p.Stderr("configured with token " + p.token)
	case ModeRejectConfigure:
		return fmt.Errorf("fake: rejected token %s", p.token)
	}
	return nil
}

func (p *Plugin) Check(ctx context.Context) (protocol.CheckResult, error) {
	p.mu.Lock()
	mode, token, prev := p.mode, p.token, p.prevToken
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
	if mode == ModeLeakToken && token != "" {
		msg := "invalid token: " + token
		if prev != "" {
			msg += " (was " + prev + ")"
		}
		res.OK = false
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemTokenLeaked, Severity: protocol.SeverityError,
			Message: msg,
		})
		p.Stderr("check failed for token " + token)
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
