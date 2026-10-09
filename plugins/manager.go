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

	"github.com/Loomux/server/credentials"
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
// Lifecycle operations on one plugin are serialised by a lock per
// plugin id, so two requests (a double-click, a boot racing an enable)
// can never leave an instance running that the Manager doesn't know.
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

	mu        sync.Mutex // guards the maps below
	instances map[string]*Instance
	checks    map[string]protocol.CheckResult
	secrets   map[string]map[string]string // the plugin's configured secrets, for redaction
	locks     map[string]*sync.Mutex       // the lifecycle lock per plugin id
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
		secrets: map[string]map[string]string{}, locks: map[string]*sync.Mutex{},
	}
}

// lock takes the plugin's lifecycle lock and returns its release.
func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
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
	defer m.lock(p.ID)()
	if err := m.store.CreatePlugin(ctx, p); err != nil {
		return nil, err
	}
	m.setSecrets(p.ID, secrets)
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
	defer m.lock(id)()
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
	m.setSecrets(id, secrets)

	inst := m.instance(id)
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
		applyErr = m.startAndCheck(ctx, p)
	}
	view, err := m.view(ctx, p)
	if err != nil {
		return nil, err
	}
	return view, applyErr
}

// Enable starts a disabled plugin (or retries one in error). A plugin
// already running is left as it is.
func (m *Manager) Enable(ctx context.Context, id string) (*View, error) {
	defer m.lock(id)()
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	var startErr error
	if inst := m.instance(id); inst == nil || inst.Status().State != StateRunning {
		p.Enabled = true
		m.setSecrets(id, p.Secrets)
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
	defer m.lock(id)()
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
// the versions match. The new version is started beside the old one and
// checked; only then is the old one stopped and the row changed. If the
// new one can't start or fails its check, it is stopped again, the old
// version keeps running, the row keeps its version, and the error is a
// *CheckFailedError.
func (m *Manager) Upgrade(ctx context.Context, id string) (*View, error) {
	defer m.lock(id)()
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
	// The new manifest may know more settings: re-apply defaults, and
	// say which version the stored configuration doesn't fit.
	schema, err := a.Manifest.Schema()
	if err != nil {
		return nil, err
	}
	cfg := schema.ApplyDefaults(mergedConfig(p.Config, p.Secrets))
	if err := schema.Validate(cfg); err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			return nil, &ConfigError{Field: ce.Field, Msg: fmt.Sprintf("%s (the stored configuration doesn't fit %s %s)", ce.Msg, a.Manifest.Name, a.Manifest.Version)}
		}
		return nil, err
	}
	plain, secrets := schema.Split(cfg)
	running := p.Enabled && p.Status != registry.PluginStatusDisabled
	var candidate *Instance
	if running {
		candidate, err = m.startInstance(ctx, p, &a.Manifest, a.Source, a.Path, mergedConfig(plain, secrets))
		if err != nil {
			reason := fmt.Sprintf("upgrade to %s: start: %s", a.Manifest.Version, m.redact(id, safeError(err)))
			m.logger.Warn("plugin upgrade failed", "plugin", p.Label, "reason", reason)
			view, verr := m.view(ctx, p)
			if verr != nil {
				return nil, verr
			}
			return view, &CheckFailedError{Reason: reason}
		}
		res, err := candidate.Check(ctx)
		if err != nil || !res.OK {
			_ = candidate.Stop(ctx)
			reason := fmt.Sprintf("upgrade to %s: ", a.Manifest.Version)
			if err != nil {
				reason += "check: " + m.redact(id, safeError(err))
			} else {
				reason += m.problemsReason(id, res.Problems)
			}
			m.logger.Warn("plugin upgrade failed", "plugin", p.Label, "reason", reason)
			view, verr := m.view(ctx, p)
			if verr != nil {
				return nil, verr
			}
			return view, &CheckFailedError{Reason: reason, Problems: m.redactProblems(id, res.Problems)}
		}
		// The new version works: swap it in and stop the old one.
		m.mu.Lock()
		old := m.instances[id]
		m.instances[id] = candidate
		m.checks[id] = m.redactResult(id, res)
		m.mu.Unlock()
		if old != nil {
			_ = old.Stop(ctx)
		}
	}
	if err := m.store.SetPluginConfig(ctx, id, plain, secrets); err != nil {
		return nil, err
	}
	p.Config, p.Secrets = plain, secrets
	m.setSecrets(id, secrets)
	p.Version, p.Protocol, p.Path, p.Trust = a.Manifest.Version, a.Manifest.Protocol, a.Path, a.Trust
	p.Capabilities = append([]string{}, a.Manifest.Capabilities...)
	p.Manifest = manifestJSON
	status := p.Status
	if running {
		status = registry.PluginStatusInstalled
	}
	if err := m.setStatus(ctx, p, status, ""); err != nil {
		return nil, err
	}
	return m.view(ctx, p)
}

// Check runs the plugin's check again and records the result. While the
// instance is restarting (a transient the supervisor is handling) the
// row is left alone and the error is ErrUnavailable.
func (m *Manager) Check(ctx context.Context, id string) (*View, error) {
	defer m.lock(id)()
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	var checkErr error
	inst := m.instance(id)
	switch {
	case inst == nil:
		checkErr = ErrUnavailable
	case inst.Status().State != StateRunning:
		checkErr = ErrUnavailable
	default:
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
	defer m.lock(id)()
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
	delete(m.secrets, id)
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
		if err := m.startOne(ctx, row.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", row.Label, err))
		}
	}
	m.updateGauge(ctx)
	return errors.Join(errs...)
}

// startOne is StartAll for one plugin, under its lock.
func (m *Manager) startOne(ctx context.Context, id string) error {
	defer m.lock(id)()
	p, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		if p == nil {
			return err
		}
		return m.fail(ctx, p, "the stored configuration doesn't decrypt with this server's master key", nil)
	}
	if inst := m.instance(id); inst != nil && inst.Status().State == StateRunning {
		return nil
	}
	m.setSecrets(id, p.Secrets)
	return m.startAndCheck(ctx, p)
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
		release := m.lock(id)
		m.stopInstance(ctx, id)
		release()
	}
	return nil
}

// startAndCheck starts p's instance (stopping any it replaces), runs its
// check, and records the outcome on the row: installed, or error with
// the reason. Callers hold p's lifecycle lock.
func (m *Manager) startAndCheck(ctx context.Context, p *registry.Plugin) error {
	manifest, err := m.installedManifest(p)
	if err != nil {
		return m.fail(ctx, p, "the installed manifest can't be read: "+safeError(err), nil)
	}
	inst, err := m.startInstance(ctx, p, manifest, p.Source, p.Path, mergedConfig(p.Config, p.Secrets))
	if err != nil {
		return m.fail(ctx, p, "start: "+m.redact(p.ID, safeError(err)), nil)
	}
	m.mu.Lock()
	old := m.instances[p.ID]
	m.instances[p.ID] = inst
	m.mu.Unlock()
	if old != nil && old != inst {
		_ = old.Stop(ctx)
	}
	return m.check(ctx, p, inst)
}

// startInstance starts an instance for p from manifest at path, without
// recording it: the caller decides whether it replaces the current one.
func (m *Manager) startInstance(ctx context.Context, p *registry.Plugin, manifest *Manifest, source registry.PluginSource, path string, config map[string]any) (*Instance, error) {
	id := p.ID
	opts := InstanceOptions{
		Label: p.Label, Source: source, Path: path, Manifest: manifest,
		Config: config, Host: m.host, Logger: m.logger,
		OnCall:  m.onCall(p.Name),
		Redact:  func(s string) string { return m.redact(id, s) },
		OnState: m.onState(id),
	}
	if m.InstanceOptions != nil {
		m.InstanceOptions(&opts)
	}
	return Start(ctx, opts)
}

// onState is what an instance reports its state changes to: a
// supervisor that gave up marks the row, and a plugin that came back
// after a transient is checked again. Both run on their own goroutine
// under the plugin's lock, since the state change may be reported while
// a lifecycle call holds it (a Stop ends with one).
func (m *Manager) onState(id string) func(i *Instance, st InstanceStatus) {
	return func(i *Instance, st InstanceStatus) {
		switch st.State {
		case StateFailed:
			go m.instanceFailed(id, i, st.Reason)
		case StateRunning:
			go m.instanceRecovered(id, i)
		}
	}
}

func (m *Manager) instanceFailed(id string, i *Instance, reason string) {
	ctx := context.Background()
	defer m.lock(id)()
	if m.instance(id) != i {
		return // replaced or stopped meanwhile
	}
	p, err := m.store.GetPlugin(ctx, id)
	if p == nil || (err != nil && errors.Is(err, registry.ErrNotFound)) {
		return
	}
	if p.Status == registry.PluginStatusDisabled {
		return
	}
	m.logger.Warn("plugin gave up", "plugin", p.Label, "reason", reason)
	_ = m.setStatus(ctx, p, registry.PluginStatusError, "the plugin stopped: "+reason)
}

func (m *Manager) instanceRecovered(id string, i *Instance) {
	ctx := context.Background()
	defer m.lock(id)()
	if m.instance(id) != i {
		return
	}
	p, err := m.store.GetPlugin(ctx, id)
	if p == nil || err != nil || p.Status != registry.PluginStatusError {
		return
	}
	// It was marked error while away and is back: let its check say.
	_ = m.check(ctx, p, i)
}

// check runs the plugin's check and records the row's status from it.
func (m *Manager) check(ctx context.Context, p *registry.Plugin, inst *Instance) error {
	res, err := inst.Check(ctx)
	if err != nil {
		return m.fail(ctx, p, "check: "+m.redact(p.ID, safeError(err)), nil)
	}
	res = m.redactResult(p.ID, res)
	m.mu.Lock()
	m.checks[p.ID] = res
	m.mu.Unlock()
	if !res.OK {
		return m.fail(ctx, p, m.problemsReason(p.ID, res.Problems), res.Problems)
	}
	return m.setStatus(ctx, p, registry.PluginStatusInstalled, "")
}

// problemsReason is a check's failure in one line: its error-severity
// messages, redacted.
func (m *Manager) problemsReason(id string, problems []protocol.Problem) string {
	var reasons []string
	for _, pr := range problems {
		if pr.Severity == protocol.SeverityError {
			reasons = append(reasons, m.redact(id, pr.Message))
		}
	}
	if len(reasons) == 0 {
		return "the plugin's check failed"
	}
	return strings.Join(reasons, "; ")
}

// fail records p as error with reason and stops its instance.
func (m *Manager) fail(ctx context.Context, p *registry.Plugin, reason string, problems []protocol.Problem) error {
	m.stopInstance(ctx, p.ID)
	if err := m.setStatus(ctx, p, registry.PluginStatusError, reason); err != nil {
		return err
	}
	m.logger.Warn("plugin failed", "plugin", p.Label, "reason", reason)
	return &CheckFailedError{Reason: reason, Problems: m.redactProblems(p.ID, problems)}
}

func (m *Manager) setStatus(ctx context.Context, p *registry.Plugin, status registry.PluginStatus, reason string) error {
	p.Status, p.StatusReason = status, reason
	if err := m.store.UpdatePlugin(ctx, p); err != nil {
		return err
	}
	m.updateGauge(ctx)
	return nil
}

// instance is the running instance for id, if any.
func (m *Manager) instance(id string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[id]
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

// setSecrets records the plugin's current secret values, which redact
// scrubs from anything the plugin produces.
func (m *Manager) setSecrets(id string, secrets map[string]string) {
	copied := make(map[string]string, len(secrets))
	for k, v := range secrets {
		copied[k] = v
	}
	m.mu.Lock()
	m.secrets[id] = copied
	m.mu.Unlock()
}

// redact scrubs the plugin's configured secret values from text it
// produced, before the text is stored, shown or logged.
func (m *Manager) redact(id, text string) string {
	m.mu.Lock()
	secrets := m.secrets[id]
	m.mu.Unlock()
	return credentials.RedactValues(text, secrets)
}

func (m *Manager) redactProblems(id string, problems []protocol.Problem) []protocol.Problem {
	if problems == nil {
		return nil
	}
	out := make([]protocol.Problem, len(problems))
	for i, p := range problems {
		p.Message = m.redact(id, p.Message)
		out[i] = p
	}
	return out
}

func (m *Manager) redactResult(id string, res protocol.CheckResult) protocol.CheckResult {
	res.Problems = m.redactProblems(id, res.Problems)
	if res.Problems == nil {
		res.Problems = []protocol.Problem{}
	}
	return res
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
	inst := m.instances[p.ID]
	m.mu.Unlock()
	if inst != nil {
		v.Instance = inst.Status()
	}
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
