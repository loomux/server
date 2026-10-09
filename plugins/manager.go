package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/registry"
)

// pluginLabel is what an installed instance's label may be.
var pluginLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidPluginLabel reports whether label can name an installed plugin.
func ValidPluginLabel(label string) bool { return pluginLabel.MatchString(label) }

// What an uninstall does with the plugin's machines (design §1.5).
const (
	UninstallDestroy = "destroy"
	UninstallKeep    = "keep"
)

// CheckFailedError: the plugin started but can't work; Reason says why
// in one line and Problems carry its check's findings, if it got that
// far.
type CheckFailedError struct {
	Reason   string
	Problems []protocol.Problem
}

func (e *CheckFailedError) Error() string { return "plugins: " + e.Reason }

// HasMachinesError: an uninstall of a plugin that still owns machines,
// with no choice made for them.
type HasMachinesError struct{ N int }

func (e *HasMachinesError) Error() string {
	return fmt.Sprintf("plugins: the plugin still owns %d machine(s); choose to destroy or keep them", e.N)
}

// CapabilityDroppedError: an upgrade whose new manifest lacks a
// capability the plugin's machines rely on.
type CapabilityDroppedError struct{ Capability string }

func (e *CapabilityDroppedError) Error() string {
	return "plugins: the new version no longer declares " + e.Capability + ", which this plugin's machines rely on"
}

// ErrBadUninstallMode is an uninstall with a targets choice that isn't
// destroy or keep.
var ErrBadUninstallMode = errors.New("plugins: targets must be destroy or keep")

// ErrBadLabel is an install with a label outside the grammar.
var ErrBadLabel = errors.New("plugins: label: lowercase letters, digits and '-' only, at most 32, starting with a letter or digit")

// InstallRequest is what Install needs.
type InstallRequest struct {
	Name   string
	Source registry.PluginSource
	Label  string
	Config map[string]any
}

// View is an installed plugin as the API shows it: the row with its
// secrets replaced by whether each is set, plus what is only known at
// run time.
type View struct {
	registry.Plugin
	// AvailableVersion is the catalog's version of the same plugin from
	// the same source, "" when it isn't in the catalog any more.
	AvailableVersion string
	// Check is the last check's result, nil if none ran.
	Check *protocol.CheckResult
	// Instance is the running instance's state; State "" when none runs.
	Instance InstanceStatus
	// Machines is how many machines the plugin owns.
	Machines int
	// SecretsUnreadable: the stored secrets don't decrypt with this
	// server's master key.
	SecretsUnreadable bool
}

// Manager installs, configures and runs plugins (design §1.5). It owns
// the rows and the running instances; nothing else touches either.
type Manager struct {
	store   registry.Store
	catalog *Catalog
	host    protocol.HostInfo
	logger  *slog.Logger
	met     *metrics.Metrics

	// MachineCounter reports how many machines a plugin instance owns.
	// Nil means none (before the target-provider capability exists).
	MachineCounter func(ctx context.Context, pluginID string) (int, error)
	// UninstallMachines deals with a plugin's machines on uninstall,
	// mode UninstallDestroy or UninstallKeep. Nil means there are none
	// to deal with.
	UninstallMachines func(ctx context.Context, pluginID, mode string) error
	// InstanceOptions, if set, adjusts every instance's options before
	// it starts (tests shorten the supervisor's timings).
	InstanceOptions func(*InstanceOptions)

	mu        sync.Mutex
	instances map[string]*Instance
	checks    map[string]protocol.CheckResult
}

// NewManager makes a Manager over store and catalog. host is what every
// plugin is told at configure; met may be nil.
func NewManager(store registry.Store, catalog *Catalog, host protocol.HostInfo, logger *slog.Logger, met *metrics.Metrics) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if met == nil {
		met = metrics.NewDiscard()
	}
	return &Manager{
		store: store, catalog: catalog, host: host, logger: logger, met: met,
		instances: map[string]*Instance{}, checks: map[string]protocol.CheckResult{},
	}
}

// Available is the catalog.
func (m *Manager) Available(ctx context.Context) ([]Available, error) {
	return m.catalog.Available(ctx)
}

// Install installs a plugin from the catalog with config, starts it,
// and checks it. The row is kept on a failure (status error) so the
// configuration can be fixed; the error then is a *CheckFailedError.
func (m *Manager) Install(ctx context.Context, r InstallRequest) (*View, error) {
	if !ValidPluginLabel(r.Label) {
		return nil, ErrBadLabel
	}
	a, err := m.catalog.Find(ctx, r.Name, r.Source)
	if err != nil {
		return nil, err
	}
	schema, err := a.Manifest.Schema()
	if err != nil {
		return nil, err
	}
	cfg := schema.ApplyDefaults(r.Config)
	if err := schema.Validate(cfg); err != nil {
		return nil, err
	}
	plain, secrets := schema.Split(cfg)
	manifestJSON, err := json.Marshal(a.Manifest)
	if err != nil {
		return nil, fmt.Errorf("plugins: manifest: %w", err)
	}
	p := &registry.Plugin{
		ID: uuid.NewString(), Name: a.Manifest.Name, Label: r.Label, Version: a.Manifest.Version,
		Protocol: a.Manifest.Protocol, Source: a.Source, Path: a.Path, Trust: a.Trust,
		Status: registry.PluginStatusInstalling, Enabled: true,
		Capabilities: append([]string{}, a.Manifest.Capabilities...), Manifest: manifestJSON,
		Config: plain, Secrets: secrets,
	}
	if err := m.store.CreatePlugin(ctx, p); err != nil {
		return nil, err
	}
	startErr := m.startAndCheck(ctx, p)
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, startErr
}

// Get is one installed plugin.
func (m *Manager) Get(ctx context.Context, id string) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil && (p == nil || errors.Is(err, registry.ErrNotFound)) {
		return nil, err
	}
	view, verr := m.view(ctx, p)
	if verr != nil {
		return nil, verr
	}
	view.SecretsUnreadable = err != nil
	return view, nil
}

// List is every installed plugin, by label.
func (m *Manager) List(ctx context.Context) ([]*View, error) {
	rows, err := m.store.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*View, 0, len(rows))
	for _, p := range rows {
		full, err := m.store.GetPlugin(ctx, p.ID)
		unreadable := err != nil
		if full == nil {
			full = p
		}
		view, err := m.view(ctx, full)
		if err != nil {
			return nil, err
		}
		view.SecretsUnreadable = unreadable
		out = append(out, view)
	}
	return out, nil
}

// SetConfig replaces the plugin's configuration: a secret left out keeps
// its stored value, "" clears it. A running plugin is reconfigured and
// checked again; one in error is started again.
func (m *Manager) SetConfig(ctx context.Context, id string, config map[string]any) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	manifest, err := m.installedManifest(p)
	if err != nil {
		return nil, err
	}
	schema, err := manifest.Schema()
	if err != nil {
		return nil, err
	}
	// Validate the configuration as it will be: the request's values,
	// with stored secrets standing in for the ones it leaves out.
	merged := schema.ApplyDefaults(config)
	for _, name := range schema.SecretFields() {
		if _, given := merged[name]; !given {
			if v, ok := p.Secrets[name]; ok {
				merged[name] = v
			}
		}
	}
	if err := schema.Validate(merged); err != nil {
		return nil, err
	}
	plain, update := schema.Split(merged)
	for name := range update {
		if _, given := config[name]; !given {
			delete(update, name) // a stored value standing in, not a change
		}
	}
	secrets := MergeSecrets(p.Secrets, update)
	if err := m.store.SetPluginConfig(ctx, id, plain, secrets); err != nil {
		return nil, err
	}
	p.Config, p.Secrets = plain, secrets

	m.mu.Lock()
	inst := m.instances[id]
	m.mu.Unlock()
	var applyErr error
	switch {
	case inst != nil && inst.Status().State == StateRunning:
		params := protocol.ConfigureParams{Config: mergedConfig(plain, secrets), Host: m.host}
		if err := inst.Call(ctx, protocol.MethodConfigure, params, nil); err != nil {
			applyErr = m.fail(ctx, p, "configure: "+safeError(err), nil)
		} else {
			applyErr = m.check(ctx, p, inst)
		}
	case p.Enabled && p.Status != registry.PluginStatusDisabled:
		m.stopInstance(ctx, id)
		applyErr = m.startAndCheck(ctx, p)
	}
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, applyErr
}

// Enable starts a disabled plugin (or retries one in error).
func (m *Manager) Enable(ctx context.Context, id string) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	inst := m.instances[id]
	m.mu.Unlock()
	var startErr error
	if inst == nil || inst.Status().State != StateRunning {
		m.stopInstance(ctx, id)
		p.Enabled = true
		startErr = m.startAndCheck(ctx, p)
	}
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, startErr
}

// Disable stops the plugin and keeps everything else. Its machines keep
// working as plain targets; only lifecycle control pauses.
func (m *Manager) Disable(ctx context.Context, id string) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	m.stopInstance(ctx, id)
	p.Enabled = false
	if err := m.setStatus(ctx, p, registry.PluginStatusDisabled, ""); err != nil {
		return nil, err
	}
	return m.view(ctx, p)
}

// Upgrade moves the plugin to the catalog's version of it. No-op when
// the versions match. The old version keeps running if the new one
// can't start.
func (m *Manager) Upgrade(ctx context.Context, id string) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := m.catalog.Find(ctx, p.Name, p.Source)
	if err != nil {
		return nil, err
	}
	if a.Manifest.Version == p.Version {
		return m.view(ctx, p)
	}
	machines, err := m.machines(ctx, id)
	if err != nil {
		return nil, err
	}
	if machines > 0 {
		for _, c := range p.Capabilities {
			if !a.Manifest.HasCapability(c) {
				return nil, &CapabilityDroppedError{Capability: c}
			}
		}
	}
	manifestJSON, err := json.Marshal(a.Manifest)
	if err != nil {
		return nil, fmt.Errorf("plugins: manifest: %w", err)
	}
	// The new manifest may know more settings: re-apply defaults.
	schema, err := a.Manifest.Schema()
	if err != nil {
		return nil, err
	}
	cfg := schema.ApplyDefaults(mergedConfig(p.Config, p.Secrets))
	if err := schema.Validate(cfg); err != nil {
		return nil, fmt.Errorf("plugins: the stored configuration doesn't fit %s %s: %w", a.Manifest.Name, a.Manifest.Version, err)
	}
	plain, secrets := schema.Split(cfg)
	if err := m.store.SetPluginConfig(ctx, id, plain, secrets); err != nil {
		return nil, err
	}
	p.Config, p.Secrets = plain, secrets
	p.Version, p.Protocol, p.Path, p.Trust = a.Manifest.Version, a.Manifest.Protocol, a.Path, a.Trust
	p.Capabilities = append([]string{}, a.Manifest.Capabilities...)
	p.Manifest = manifestJSON
	m.stopInstance(ctx, id)
	var startErr error
	if p.Enabled && p.Status != registry.PluginStatusDisabled {
		startErr = m.startAndCheck(ctx, p)
	} else if err := m.store.UpdatePlugin(ctx, p); err != nil {
		return nil, err
	}
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, startErr
}

// Check runs the plugin's check again and records the result.
func (m *Manager) Check(ctx context.Context, id string) (*View, error) {
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	inst := m.instances[id]
	m.mu.Unlock()
	var checkErr error
	if inst == nil {
		checkErr = ErrUnavailable
	} else {
		checkErr = m.check(ctx, p, inst)
	}
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, checkErr
}

// Uninstall stops the plugin, deals with its machines as targets says
// (required when it has any), erases its secrets and removes the row.
func (m *Manager) Uninstall(ctx context.Context, id, targets string) error {
	if targets != "" && targets != UninstallDestroy && targets != UninstallKeep {
		return ErrBadUninstallMode
	}
	if _, err := m.store.GetPlugin(ctx, id); err != nil && errors.Is(err, registry.ErrNotFound) {
		return err
	}
	n, err := m.machines(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		if targets == "" {
			return &HasMachinesError{N: n}
		}
		if m.UninstallMachines != nil {
			if err := m.UninstallMachines(ctx, id, targets); err != nil {
				return err
			}
		}
	}
	m.stopInstance(ctx, id)
	if err := m.store.SetPluginConfig(ctx, id, map[string]any{}, nil); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if err := m.store.DeletePlugin(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.checks, id)
	m.mu.Unlock()
	m.updateGauge(ctx)
	return nil
}

// StartAll starts every enabled plugin at boot. A failure marks its
// row and goes on; the error returned is them joined, for the log.
func (m *Manager) StartAll(ctx context.Context) error {
	rows, err := m.store.ListPlugins(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if !row.Enabled || row.Status == registry.PluginStatusDisabled {
			continue
		}
		p, err := m.store.GetPlugin(ctx, row.ID)
		if err != nil {
			if p == nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, m.fail(ctx, p, "the stored configuration doesn't decrypt with this server's master key", nil))
			continue
		}
		if err := m.startAndCheck(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Label, err))
		}
	}
	m.updateGauge(ctx)
	return errors.Join(errs...)
}

// HealthReport is the plugin system's health for GET /health/deep.
type HealthReport struct {
	// Status is healthy (every enabled plugin runs and checks ok, or
	// none is installed) or degraded.
	Status  string
	Error   string
	Plugins map[string]PluginHealth
}

// PluginHealth is one plugin's line in the report.
type PluginHealth struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Instance string `json:"instance,omitempty"`
	Reason   string `json:"reason,omitempty"`
	CheckOK  bool   `json:"check_ok"`
	Machines int    `json:"machines"`
}

// Health reports every installed plugin.
func (m *Manager) Health(ctx context.Context) HealthReport {
	report := HealthReport{Status: "healthy", Plugins: map[string]PluginHealth{}}
	views, err := m.List(ctx)
	if err != nil {
		return HealthReport{Status: "unhealthy", Error: safeError(err), Plugins: map[string]PluginHealth{}}
	}
	var bad []string
	for _, v := range views {
		h := PluginHealth{Name: v.Name, Status: string(v.Status), Instance: v.Instance.State, Reason: v.Instance.Reason, Machines: v.Machines}
		if v.Check != nil {
			h.CheckOK = v.Check.OK
		}
		if h.Reason == "" {
			h.Reason = v.StatusReason
		}
		report.Plugins[v.Label] = h
		if v.Enabled && (v.Status != registry.PluginStatusInstalled || v.Instance.State != StateRunning || (v.Check != nil && !v.Check.OK)) {
			bad = append(bad, v.Label)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		report.Status = "degraded"
		report.Error = "not running or failing checks: " + strings.Join(bad, ", ")
	}
	return report
}

// Close stops every running plugin.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.stopInstance(ctx, id)
	}
	return nil
}

// startAndCheck starts p's instance and runs its check, recording the
// outcome on the row: installed, or error with the reason.
func (m *Manager) startAndCheck(ctx context.Context, p *registry.Plugin) error {
	manifest, err := m.installedManifest(p)
	if err != nil {
		return m.fail(ctx, p, "the installed manifest can't be read: "+safeError(err), nil)
	}
	opts := InstanceOptions{
		Label: p.Label, Source: p.Source, Path: p.Path, Manifest: manifest,
		Config: mergedConfig(p.Config, p.Secrets), Host: m.host, Logger: m.logger,
		OnCall: m.onCall(p.Name),
	}
	if m.InstanceOptions != nil {
		m.InstanceOptions(&opts)
	}
	inst, err := Start(ctx, opts)
	if err != nil {
		return m.fail(ctx, p, "start: "+safeError(err), nil)
	}
	m.mu.Lock()
	m.instances[p.ID] = inst
	m.mu.Unlock()
	return m.check(ctx, p, inst)
}

// check runs the plugin's check and records the row's status from it.
func (m *Manager) check(ctx context.Context, p *registry.Plugin, inst *Instance) error {
	res, err := inst.Check(ctx)
	if err != nil {
		return m.fail(ctx, p, "check: "+safeError(err), nil)
	}
	m.mu.Lock()
	m.checks[p.ID] = res
	m.mu.Unlock()
	if !res.OK {
		var reasons []string
		for _, pr := range res.Problems {
			if pr.Severity == protocol.SeverityError {
				reasons = append(reasons, pr.Message)
			}
		}
		if len(reasons) == 0 {
			reasons = []string{"the plugin's check failed"}
		}
		return m.fail(ctx, p, strings.Join(reasons, "; "), res.Problems)
	}
	return m.setStatus(ctx, p, registry.PluginStatusInstalled, "")
}

// fail records p as error with reason and stops its instance.
func (m *Manager) fail(ctx context.Context, p *registry.Plugin, reason string, problems []protocol.Problem) error {
	m.stopInstance(ctx, p.ID)
	if err := m.setStatus(ctx, p, registry.PluginStatusError, reason); err != nil {
		return err
	}
	m.logger.Warn("plugin failed", "plugin", p.Label, "reason", reason)
	return &CheckFailedError{Reason: reason, Problems: problems}
}

func (m *Manager) setStatus(ctx context.Context, p *registry.Plugin, status registry.PluginStatus, reason string) error {
	p.Status, p.StatusReason = status, reason
	if err := m.store.UpdatePlugin(ctx, p); err != nil {
		return err
	}
	m.updateGauge(ctx)
	return nil
}

func (m *Manager) stopInstance(ctx context.Context, id string) {
	m.mu.Lock()
	inst := m.instances[id]
	delete(m.instances, id)
	m.mu.Unlock()
	if inst != nil {
		_ = inst.Stop(ctx)
	}
}

func (m *Manager) machines(ctx context.Context, id string) (int, error) {
	if m.MachineCounter == nil {
		return 0, nil
	}
	return m.MachineCounter(ctx, id)
}

// view builds the API's view of p.
func (m *Manager) view(ctx context.Context, p *registry.Plugin) (*View, error) {
	v := &View{Plugin: *p}
	v.Secrets = nil
	v.Config = map[string]any{}
	for k, val := range p.Config {
		v.Config[k] = val
	}
	if manifest, err := m.installedManifest(p); err == nil {
		if schema, err := manifest.Schema(); err == nil {
			for _, name := range schema.SecretFields() {
				_, set := p.Secrets[name]
				v.Config[name] = map[string]any{"set": set}
			}
		}
	}
	if a, err := m.catalog.Find(ctx, p.Name, p.Source); err == nil {
		v.AvailableVersion = a.Manifest.Version
	}
	m.mu.Lock()
	if res, ok := m.checks[p.ID]; ok {
		r := res
		v.Check = &r
	}
	if inst := m.instances[p.ID]; inst != nil {
		v.Instance = inst.Status()
	}
	m.mu.Unlock()
	n, err := m.machines(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	v.Machines = n
	return v, nil
}

func (m *Manager) installedManifest(p *registry.Plugin) (*Manifest, error) {
	if len(p.Manifest) == 0 {
		return nil, errors.New("plugins: no installed manifest")
	}
	return ParseManifest(p.Manifest)
}

func (m *Manager) onCall(name string) func(method string, err error) {
	return func(method string, err error) {
		outcome := "ok"
		var rpcErr *rpc.Error
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded):
			outcome = "timeout"
		case errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeUnavailable:
			outcome = "unavailable"
		default:
			outcome = "error"
		}
		m.met.PluginCalls.WithLabelValues(name, method, outcome).Inc()
	}
}

// updateGauge recomputes loomux_plugins from the rows.
func (m *Manager) updateGauge(ctx context.Context) {
	rows, err := m.store.ListPlugins(ctx)
	if err != nil {
		return
	}
	m.met.PluginsByStatus.Reset()
	counts := map[[2]string]int{}
	for _, p := range rows {
		counts[[2]string{p.Name, string(p.Status)}]++
	}
	for k, n := range counts {
		m.met.PluginsByStatus.WithLabelValues(k[0], k[1]).Set(float64(n))
	}
}

// mergedConfig is the whole configuration a plugin is given.
func mergedConfig(plain map[string]any, secrets map[string]string) map[string]any {
	out := make(map[string]any, len(plain)+len(secrets))
	for k, v := range plain {
		out[k] = v
	}
	for k, v := range secrets {
		out[k] = v
	}
	return out
}

// safeError is an error's text for a row or a response: bounded, one
// line.
func safeError(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}
