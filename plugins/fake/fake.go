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
	"github.com/Loomux/server/plugins/rpc"
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
	tmode     string
	domain    string
	targets   *targetsState
}

// New returns a fake in mode ok.
func New() *Plugin {
	return &Plugin{
		Exit:     os.Exit,
		Stderr:   func(line string) { fmt.Fprintln(os.Stderr, line) },
		mode:     ModeOK,
		greeting: "hi",
		tmode:    TargetsOK,
		domain:   "fake.invalid",
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
	if tm, ok := cp.Config["targets_mode"].(string); ok && tm != "" {
		p.tmode = tm
	}
	if d, ok := cp.Config["address_domain"].(string); ok && d != "" {
		p.domain = d
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

// In-memory machines (the targets.* group) for the host's tests.

// Targets modes ("targets_mode").
const (
	TargetsOK          = "ok"
	TargetsCreateFails = "create_fails"
	TargetsVanish      = "vanish"
	TargetsSlowStart   = "slow_start"
)

// machine is one in-memory environment.
type machine struct {
	env     protocol.Environment
	spec    protocol.EnvironmentSpec
	gets    int
	started time.Time
}

// targetsState is the fake's machines; nil until the first targets.*
// call.
type targetsState struct {
	mu       sync.Mutex
	machines map[string]*machine
}

func (p *Plugin) targetsMode() (mode, domain, instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tmode, p.domain, p.host.InstanceID
}

func (p *Plugin) state() *targetsState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.targets == nil {
		p.targets = &targetsState{machines: map[string]*machine{}}
	}
	return p.targets
}

func notFound(id string) error {
	return &rpc.Error{Code: rpc.CodeNotFound, Message: "no machine " + id}
}

func (p *Plugin) DescribeTargets(ctx context.Context) (protocol.TargetsInfo, error) {
	_, domain, _ := p.targetsMode()
	st := p.state()
	st.mu.Lock()
	n := len(st.machines)
	st.mu.Unlock()
	return protocol.TargetsInfo{
		Sizes:             []protocol.Size{{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"}, {Name: "medium", CPU: "2", Memory: "4Gi", Disk: "20Gi"}},
		PersistentDefault: true,
		EgressOptions:     []string{protocol.EgressInternet, protocol.EgressNone},
		Image:             "ghcr.io/loomux/agent:fake",
		MaxEnvironments:   5,
		Environments:      n,
		AddressTemplate:   "lx-{id}." + domain,
		SSHProxy:          protocol.SSHProxyNone,
		User:              "agent",
		Port:              2222,
	}, nil
}

func (p *Plugin) CreateTarget(ctx context.Context, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	mode, domain, _ := p.targetsMode()
	if mode == TargetsCreateFails {
		return protocol.Environment{}, &rpc.Error{Code: rpc.CodeQuota, Message: "the fake refuses to create machines in this mode"}
	}
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	if m, ok := st.machines[spec.ID]; ok {
		return m.env, nil // idempotent
	}
	m := &machine{spec: spec, started: time.Now(), env: protocol.Environment{
		ID: spec.ID, Status: protocol.EnvStarting,
		Address: protocol.Address{Host: "lx-" + spec.ID + "." + domain, Port: 2222, Proxy: protocol.SSHProxyNone},
		Size:    spec.Size, Persistent: spec.Persistent, Egress: spec.Egress, CreatedAt: time.Now().UTC(),
		ImageDigest: "sha256:fake",
	}}
	st.machines[spec.ID] = m
	return m.env, nil
}

func (p *Plugin) GetTarget(ctx context.Context, id string) (protocol.Environment, error) {
	mode, _, _ := p.targetsMode()
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.machines[id]
	if !ok {
		return protocol.Environment{}, notFound(id)
	}
	m.gets++
	switch m.env.Status {
	case protocol.EnvStarting:
		need := 1
		if mode == TargetsSlowStart {
			need = 3
		}
		if m.gets >= need {
			m.env.Status = protocol.EnvRunning
			m.started = time.Now()
		}
	case protocol.EnvRunning:
		if mode == TargetsVanish && time.Since(m.started) > time.Second {
			delete(st.machines, id)
			return protocol.Environment{}, notFound(id)
		}
	}
	return m.env, nil
}

func (p *Plugin) ListTargets(ctx context.Context) ([]protocol.Environment, error) {
	_, _, instance := p.targetsMode()
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []protocol.Environment{}
	for _, m := range st.machines {
		if m.spec.Labels[protocol.LabelInstance] == instance {
			out = append(out, m.env)
		}
	}
	return out, nil
}

func (p *Plugin) StartTarget(ctx context.Context, id string) error {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.machines[id]
	if !ok {
		return notFound(id)
	}
	if m.env.Status == protocol.EnvStopped || m.env.Status == protocol.EnvLost {
		m.env.Status, m.gets = protocol.EnvStarting, 0
	}
	return nil
}

func (p *Plugin) StopTarget(ctx context.Context, id string) error {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.machines[id]
	if !ok {
		return notFound(id)
	}
	m.env.Status = protocol.EnvStopped
	return nil
}

func (p *Plugin) RecreateTarget(ctx context.Context, id string, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.machines[id]
	if !ok {
		return protocol.Environment{}, notFound(id)
	}
	m.spec = spec
	m.env.Size, m.env.Status, m.gets = spec.Size, protocol.EnvStarting, 0
	m.env.ImageDigest = "sha256:fake-" + spec.Image
	return m.env, nil
}

func (p *Plugin) DestroyTarget(ctx context.Context, id string) error {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.machines, id) // idempotent
	return nil
}

func (p *Plugin) TargetHealth(ctx context.Context, id string) (protocol.EnvironmentHealth, error) {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.machines[id]
	if !ok {
		return protocol.EnvironmentHealth{}, notFound(id)
	}
	return protocol.EnvironmentHealth{Status: m.env.Status, ImageDigest: m.env.ImageDigest}, nil
}

func (p *Plugin) TargetAttachCommands(ctx context.Context, id, session string) ([]protocol.AttachCommand, error) {
	st := p.state()
	st.mu.Lock()
	_, ok := st.machines[id]
	st.mu.Unlock()
	if !ok {
		return nil, notFound(id)
	}
	return []protocol.AttachCommand{{Via: "fake", Command: "fake attach " + id + " -- tmux -L loomux attach -t " + session}}, nil
}

// Machines is a snapshot of the fake's machines, for tests.
func (p *Plugin) Machines() map[string]protocol.Environment {
	st := p.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]protocol.Environment{}
	for id, m := range st.machines {
		out[id] = m.env
	}
	return out
}
