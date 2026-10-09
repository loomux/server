package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// job is one background job (a create, or a follow after start or
// recreate). The map entry is the job itself, so a job's cleanup never
// removes a newer job's entry.
type job struct {
	cancel context.CancelFunc
}

// errStale is a job's signal that the machine moved on while it wasn't
// looking (a Stop, a delete, an uninstall), so its result is dropped.
var errStale = errors.New("providers: the machine moved on")

// inFlight is whether a job follows a machine in status s.
func inFlight(s registry.EnvironmentStatus) bool {
	switch s {
	case registry.EnvironmentCreating, registry.EnvironmentStarting, registry.EnvironmentRecreating:
		return true
	}
	return false
}

// Defaults.
const (
	DefaultCreateTimeout = 10 * time.Minute
	DefaultPollInterval  = 2 * time.Second
	DefaultOrphanTTL     = 24 * time.Hour
	// readyTimeout bounds EnsureRunning's wait for a machine to come up.
	readyTimeout = 5 * time.Minute
)

// Manager turns machines a plugin makes into targets and keeps them in
// step with the plugin (design §1.9). Every mutation of one machine is
// serialised by a lock per environment id.
type Manager struct {
	store      registry.Store
	plugins    *plugins.Manager
	logger     *slog.Logger
	met        *metrics.Metrics
	instanceID string

	// Probe runs the target health probe once a machine is running (the
	// router's ProbeTarget); nil skips it.
	Probe func(ctx context.Context, targetID string) error
	// ActiveTask reports whether a task is mid-turn or taken over on the
	// target, which refuses stop, recreate and destroy; nil means never.
	ActiveTask func(ctx context.Context, targetID string) (bool, error)
	// DropSSHKey stops serving a deleted key (the agent pool); nil skips.
	DropSSHKey func(keyID string)

	CreateTimeout time.Duration
	PollInterval  time.Duration
	OrphanTTL     time.Duration
	Now           func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	jobs  sync.WaitGroup
	// running is the background job following each machine, if any.
	running map[string]*job
	// orphanSeen is when an orphan was first listed, keyed plugin/env.
	orphanSeen map[string]time.Time
	closed     bool
}

// NewManager makes a Manager over store and the plugin manager.
// instanceID labels every spec as this server's.
func NewManager(store registry.Store, pm *plugins.Manager, instanceID string, logger *slog.Logger, met *metrics.Metrics) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if met == nil {
		met = metrics.NewDiscard()
	}
	return &Manager{
		store: store, plugins: pm, logger: logger, met: met, instanceID: instanceID,
		CreateTimeout: DefaultCreateTimeout, PollInterval: DefaultPollInterval, OrphanTTL: DefaultOrphanTTL,
		Now:   time.Now,
		locks: map[string]*sync.Mutex{}, running: map[string]*job{}, orphanSeen: map[string]time.Time{},
	}
}

func (m *Manager) lock(envID string) func() {
	m.mu.Lock()
	l := m.locks[envID]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[envID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// CreateRequest is what Create needs: the target as the API decoded it
// (name, policy, permission mode) and the machine's shape.
type CreateRequest struct {
	Target     registry.Target
	PluginID   string
	Size       string
	Persistent *bool
	Egress     string
}

// Info is what the plugin offers (targets.describe).
func (m *Manager) Info(ctx context.Context, pluginID string) (protocol.TargetsInfo, error) {
	var info protocol.TargetsInfo
	if err := m.plugins.Call(ctx, pluginID, protocol.MethodTargetsDescribe, nil, &info); err != nil {
		return info, m.mapPluginErr(err)
	}
	return info, nil
}

// mapPluginErr turns a plugin-manager error into this package's.
func (m *Manager) mapPluginErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, plugins.ErrUnavailable), errors.Is(err, registry.ErrNotFound):
		return ErrPluginUnavailable
	}
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeQuota {
		return fmt.Errorf("%w: %s", ErrQuota, rpcErr.Message)
	}
	return err
}

// requireRunning says the plugin instance is installed, enabled and
// running.
func (m *Manager) requireRunning(ctx context.Context, pluginID string) (*plugins.View, error) {
	v, err := m.plugins.Get(ctx, pluginID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, ErrPluginUnavailable
		}
		return nil, err
	}
	if v.Status != registry.PluginStatusInstalled || !v.Enabled || v.Instance.State != plugins.StateRunning {
		return nil, ErrPluginUnavailable
	}
	return v, nil
}

// Create registers the target and the environment (rows first), then
// asks the plugin for the machine in the background. It returns once the
// rows exist; the target becomes ready as the machine comes up.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*registry.Target, *registry.Environment, error) {
	view, err := m.requireRunning(ctx, req.PluginID)
	if err != nil {
		return nil, nil, err
	}
	info, err := m.Info(ctx, req.PluginID)
	if err != nil {
		return nil, nil, err
	}
	caps := map[string]bool{}
	for _, c := range view.Capabilities {
		caps[c] = true
	}
	if !caps[protocol.CapTargetsCreate] {
		return nil, nil, &plugins.CapabilityMissingError{Method: protocol.MethodTargetsCreate, Capability: protocol.CapTargetsCreate}
	}
	size := req.Size
	if size == "" && len(info.Sizes) > 0 {
		size = info.Sizes[0].Name
	}
	if !hasSize(info, size) {
		return nil, nil, &InvalidError{Field: "size", Msg: "must be one of " + sizeNames(info)}
	}
	persistent := info.PersistentDefault
	if req.Persistent != nil {
		persistent = *req.Persistent
	}
	if persistent && !caps[protocol.CapTargetsPersistent] && caps[protocol.CapTargetsEphemeral] {
		return nil, nil, &InvalidError{Field: "persistent", Msg: "this plugin only makes ephemeral machines"}
	}
	if !persistent && !caps[protocol.CapTargetsEphemeral] {
		return nil, nil, &InvalidError{Field: "persistent", Msg: "this plugin only makes machines that keep their data"}
	}
	egress := req.Egress
	if len(info.EgressOptions) == 0 {
		if egress != "" {
			return nil, nil, &InvalidError{Field: "egress", Msg: "this plugin offers no egress choice right now"}
		}
		egress = protocol.EgressInternet
	} else {
		if egress == "" {
			egress = info.EgressOptions[0]
		}
		if !contains(info.EgressOptions, egress) {
			return nil, nil, &InvalidError{Field: "egress", Msg: "must be one of " + strings.Join(info.EgressOptions, ", ")}
		}
	}
	if info.MaxEnvironments > 0 {
		existing, err := m.store.ListEnvironmentsByPlugin(ctx, req.PluginID)
		if err != nil {
			return nil, nil, err
		}
		if len(existing) >= info.MaxEnvironments {
			return nil, nil, fmt.Errorf("%w (%d)", ErrQuota, info.MaxEnvironments)
		}
	}
	if info.Port < 1 || info.Port > 65535 || info.User == "" {
		return nil, nil, &BadAddressError{Detail: "the plugin's describe gives no usable sshd user and port"}
	}

	id, err := newID()
	if err != nil {
		return nil, nil, err
	}
	host := expandTemplate(info.AddressTemplate, id)
	if err := checkAddress(protocol.Address{Host: host, Port: info.Port, Proxy: info.SSHProxy}); err != nil {
		return nil, nil, err
	}

	// The target's own key, as generate_ssh_key makes one.
	key, err := m.generateKey(ctx, req.Target.Name)
	if err != nil {
		return nil, nil, err
	}
	hostKey, err := generateHostKey(id)
	if err != nil {
		m.releaseKey(ctx, key.ID)
		return nil, nil, err
	}

	target := req.Target
	target.ID = uuid.NewString()
	target.Kind = registry.TargetKindRemote
	target.Host, target.User, target.SSHPort = host, info.User, info.Port
	target.SSHKeyRef = key.ID
	target.SSHProxy = ""
	if info.SSHProxy == protocol.SSHProxyNone {
		target.SSHProxy = registry.SSHProxyNone
	}
	target.HostKeys = pinLine(host, info.Port, hostKey.pubKey)
	target.WorkspaceRoot = WorkspaceRoot
	if err := target.Validate(); err != nil {
		m.releaseKey(ctx, key.ID)
		return nil, nil, &InvalidError{Field: "target", Msg: err.Error()}
	}
	if err := m.store.CreateTarget(ctx, &target); err != nil {
		m.releaseKey(ctx, key.ID)
		return nil, nil, err
	}
	env := &registry.Environment{
		ID: id, TargetID: target.ID, PluginID: req.PluginID, PluginName: view.Name, PluginVersion: view.Version,
		Status: registry.EnvironmentCreating, Size: size, Persistent: persistent, Egress: egress, Image: info.Image,
		HostKey: hostKey.public, HostPrivateKey: hostKey.privatePEM,
	}
	if err := m.store.CreateEnvironment(ctx, env); err != nil {
		_ = m.store.DeleteTarget(ctx, target.ID)
		m.releaseKey(ctx, key.ID)
		return nil, nil, err
	}
	m.updateGauge(ctx)
	m.startJob(env.ID, func(ctx context.Context) { m.runCreate(ctx, env.ID) })
	return &target, env, nil
}

func hasSize(info protocol.TargetsInfo, name string) bool {
	for _, s := range info.Sizes {
		if s.Name == name {
			return true
		}
	}
	return false
}

func sizeNames(info protocol.TargetsInfo) string {
	names := make([]string, len(info.Sizes))
	for i, s := range info.Sizes {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// generateKey makes and stores the target's managed key (origin target).
func (m *Manager) generateKey(ctx context.Context, targetName string) (*registry.SSHKey, error) {
	name := keyNameFor(targetName)
	for attempt := 0; ; attempt++ {
		k, err := targets.GenerateSSHKey(uuid.NewString(), name)
		if err != nil {
			return nil, err
		}
		k.Origin = registry.SSHKeyOriginTarget
		err = m.store.CreateSSHKey(ctx, k)
		switch {
		case err == nil:
			return k, nil
		case errors.Is(err, registry.ErrConflict) && attempt < 3:
			name = keyNameFor(targetName) + "-" + uuid.NewString()[:8]
		default:
			return nil, err
		}
	}
}

// keyNameFor is the key's name from the target's: the key-name grammar.
func keyNameFor(targetName string) string {
	var b strings.Builder
	for _, r := range targetName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "._-")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" || !targets.ValidSSHKeyName(name) {
		name = "machine"
	}
	return name
}

// releaseKey deletes a key made for a machine, best effort.
func (m *Manager) releaseKey(ctx context.Context, keyID string) {
	if keyID == "" {
		return
	}
	if err := m.store.DeleteSSHKey(ctx, keyID); err == nil && m.DropSSHKey != nil {
		m.DropSSHKey(keyID)
	}
}

// buildSpec is the plugin's view of a machine, from its rows.
func (m *Manager) buildSpec(ctx context.Context, env *registry.Environment, target *registry.Target, image string) (protocol.EnvironmentSpec, error) {
	key, err := m.store.GetSSHKey(ctx, target.SSHKeyRef)
	if err != nil {
		return protocol.EnvironmentSpec{}, fmt.Errorf("providers: the target's key: %w", err)
	}
	if env.HostPrivateKey == nil {
		full, err := m.store.GetEnvironment(ctx, env.ID)
		if err != nil {
			return protocol.EnvironmentSpec{}, err
		}
		env.HostPrivateKey = full.HostPrivateKey
	}
	if image == "" {
		image = env.Image
	}
	return protocol.EnvironmentSpec{
		ID: env.ID, TargetID: target.ID, Name: target.Name, Size: env.Size, Persistent: env.Persistent,
		Egress: env.Egress, Image: image,
		SSH: protocol.SSHBootstrap{
			AuthorizedKey: authorizedKeyLine(key), HostPrivateKey: string(env.HostPrivateKey),
			HostPublicKey: env.HostKey, Port: target.SSHPort,
		},
		Labels: map[string]string{
			protocol.LabelInstance: m.instanceID, protocol.LabelTarget: target.ID, protocol.LabelEnvironment: env.ID,
		},
	}, nil
}

// startJob runs fn on its own goroutine, bounded by CreateTimeout, and
// replacing any job for the same environment.
func (m *Manager) startJob(envID string, fn func(ctx context.Context)) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if old := m.running[envID]; old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.CreateTimeout)
	j := &job{cancel: cancel}
	m.running[envID] = j
	m.jobs.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.jobs.Done()
		defer cancel()
		fn(ctx)
		m.mu.Lock()
		// Only this job's own entry: a newer job may have replaced it.
		if m.running[envID] == j {
			delete(m.running, envID)
		}
		m.mu.Unlock()
	}()
}

// following reports whether a job is following the machine.
func (m *Manager) following(envID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[envID] != nil
}

// runCreate asks the plugin for the machine and follows it to running.
func (m *Manager) runCreate(ctx context.Context, envID string) {
	env, target, err := m.load(ctx, envID)
	if err != nil {
		return
	}
	spec, err := m.buildSpec(ctx, env, target, "")
	if err != nil {
		m.failJob(ctx, envID, "prepare: "+err.Error())
		return
	}
	var pe protocol.Environment
	if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsCreate, spec, &pe); err != nil {
		m.failJob(ctx, envID, "create: "+safeErr(err))
		return
	}
	if err := m.applyLocked(ctx, envID, pe); err != nil {
		if !errors.Is(err, errStale) {
			m.failJob(ctx, envID, err.Error())
		}
		return
	}
	m.follow(ctx, envID)
}

// applyLocked is apply under the environment's lock, for the jobs. The
// row is re-read there: what the job saw before may have been overtaken
// by a Stop, a delete or an uninstall, and then the plugin's word is
// dropped (errStale) rather than written over theirs.
func (m *Manager) applyLocked(ctx context.Context, envID string, pe protocol.Environment) error {
	defer m.lock(envID)()
	env, target, err := m.load(ctx, envID)
	if err != nil {
		return err
	}
	if !inFlight(env.Status) {
		return errStale
	}
	return m.apply(ctx, env, target, pe)
}

// follow polls the plugin until the machine is running (then probes the
// target), or ends in error or lost, or the job's deadline passes. A
// job cancelled on purpose leaves the row as the canceller set it.
func (m *Manager) follow(ctx context.Context, envID string) {
	ticker := time.NewTicker(m.PollInterval)
	defer ticker.Stop()
	for {
		env, _, err := m.load(ctx, envID)
		if err != nil || !inFlight(env.Status) {
			return
		}
		var pe protocol.Environment
		err = m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: env.ID}, &pe)
		switch {
		case err == nil:
			if err := m.applyLocked(ctx, envID, pe); err != nil {
				if !errors.Is(err, errStale) {
					m.failJob(ctx, envID, err.Error())
				}
				return
			}
			if pe.Status == protocol.EnvRunning {
				return
			}
		case isNotFound(err):
			m.failJob(ctx, envID, "the plugin lost the machine while it was coming up")
			return
		default:
			// Unavailable or transient: keep polling until the deadline.
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				m.failJob(ctx, envID, "the machine didn't become ready within "+m.CreateTimeout.String())
			}
			return
		case <-ticker.C:
		}
	}
}

// load reads an environment (with its host key) and its target.
func (m *Manager) load(ctx context.Context, envID string) (*registry.Environment, *registry.Target, error) {
	env, err := m.store.GetEnvironment(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	target, err := m.store.GetTarget(ctx, env.TargetID)
	if err != nil {
		return nil, nil, err
	}
	return env, target, nil
}

// apply records what the plugin says about a machine: its status, its
// digest, and its address (which moves the target's host, port and pin
// when it differs from what the template promised). The caller holds
// the environment's lock.
func (m *Manager) apply(ctx context.Context, env *registry.Environment, target *registry.Target, pe protocol.Environment) error {
	if pe.ID != "" && pe.ID != env.ID {
		return &BadAddressError{Detail: "the plugin answered for another machine"}
	}
	if err := checkAddress(pe.Address); err != nil {
		return err
	}
	if pe.Address.Host != target.Host || pe.Address.Port != target.SSHPort {
		pub, err := parsePublicKey(env.HostKey)
		if err != nil {
			return err
		}
		target.Host, target.SSHPort = pe.Address.Host, pe.Address.Port
		target.HostKeys = pinLine(pe.Address.Host, pe.Address.Port, pub)
		if err := target.Validate(); err != nil {
			return &BadAddressError{Detail: err.Error()}
		}
		if err := m.store.UpdateTarget(ctx, target); err != nil {
			return err
		}
		if err := m.store.SetTargetHostKeys(ctx, target.ID, target.HostKeys); err != nil {
			return err
		}
	}
	status, ok := mapStatus(pe.Status)
	if !ok {
		return &BadAddressError{Detail: "status " + strconv.Quote(truncate(pe.Status, 40))}
	}
	was := env.Status
	if pe.ImageDigest != "" {
		env.ImageDigest = truncate(pe.ImageDigest, 200)
	}
	env.Status, env.StatusReason = status, truncate(strings.Join(strings.Fields(pe.Reason), " "), 500)
	if err := m.store.UpdateEnvironment(ctx, env); err != nil {
		return err
	}
	m.updateGauge(ctx)
	if status == registry.EnvironmentRunning && was != registry.EnvironmentRunning {
		m.logger.Info("machine running", "target", target.Name, "environment", env.ID, "host", target.Host)
		if m.Probe != nil {
			if err := m.Probe(ctx, target.ID); err != nil {
				m.logger.Warn("probe after machine came up", "target", target.Name, "error", err.Error())
			}
		}
	}
	return nil
}

func mapStatus(s string) (registry.EnvironmentStatus, bool) {
	for _, known := range protocol.EnvironmentStatuses {
		if s == known {
			return registry.EnvironmentStatus(s), true
		}
	}
	return "", false
}

// failJob records a job's failure: the machine becomes error with
// reason. Not when the job was cancelled on purpose (a Stop, a delete,
// an uninstall, shutdown, a newer job): the canceller set the row, and
// after a shutdown reconcile resumes the machine. Not when the machine
// is no longer in flight: someone moved it on. The row is re-read under
// its lock with a fresh context, since the job's own may be the reason.
func (m *Manager) failJob(ctx context.Context, envID, reason string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer m.lock(envID)()
	env, err := m.store.GetEnvironment(ctx, envID)
	if err != nil || !inFlight(env.Status) {
		return
	}
	env.Status, env.StatusReason = registry.EnvironmentError, truncate(reason, 500)
	if err := m.store.UpdateEnvironment(ctx, env); err != nil {
		m.logger.Warn("record machine error", "environment", envID, "error", err.Error())
	}
	m.logger.Warn("machine failed", "environment", envID, "reason", env.StatusReason)
	m.updateGauge(ctx)
}

func isNotFound(err error) bool {
	var rpcErr *rpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeNotFound
}

func safeErr(err error) string {
	return truncate(strings.Join(strings.Fields(err.Error()), " "), 500)
}

// Get is the environment behind targetID; ErrNotMachine for a target no
// plugin made.
func (m *Manager) Get(ctx context.Context, targetID string) (*registry.Environment, error) {
	env, err := m.store.GetEnvironmentByTarget(ctx, targetID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, ErrNotMachine
		}
		if env == nil {
			return nil, err
		}
	}
	return env, nil
}

// IsMachine reports whether a plugin made the target.
func (m *Manager) IsMachine(ctx context.Context, targetID string) bool {
	_, err := m.store.GetEnvironmentByTarget(ctx, targetID)
	return err == nil || !errors.Is(err, registry.ErrNotFound)
}

// managed loads a machine for a lifecycle operation: its rows, and its
// plugin running.
func (m *Manager) managed(ctx context.Context, targetID string) (*registry.Environment, *registry.Target, error) {
	env, err := m.Get(ctx, targetID)
	if err != nil {
		return nil, nil, err
	}
	target, err := m.store.GetTarget(ctx, targetID)
	if err != nil {
		return nil, nil, err
	}
	if env.Status == registry.EnvironmentDetached || env.PluginID == "" {
		return env, target, ErrDetached
	}
	if _, err := m.requireRunning(ctx, env.PluginID); err != nil {
		return env, target, err
	}
	return env, target, nil
}

// reload re-reads a machine once its lock is held: what managed() saw
// may have moved on while the lock was being taken.
func (m *Manager) reload(ctx context.Context, envID string) (*registry.Environment, *registry.Target, error) {
	env, target, err := m.load(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	if env.Status == registry.EnvironmentDetached || env.PluginID == "" {
		return env, target, ErrDetached
	}
	return env, target, nil
}

func (m *Manager) refuseIfActive(ctx context.Context, targetID string) error {
	if m.ActiveTask == nil {
		return nil
	}
	active, err := m.ActiveTask(ctx, targetID)
	if err != nil {
		return err
	}
	if active {
		return ErrTaskActive
	}
	return nil
}

// Stop frees the machine's compute, keeping its data.
func (m *Manager) Stop(ctx context.Context, targetID string) error {
	env, _, err := m.managed(ctx, targetID)
	if err != nil {
		return err
	}
	defer m.lock(env.ID)()
	if env, _, err = m.reload(ctx, env.ID); err != nil {
		return err
	}
	if !env.Persistent {
		return ErrEphemeralStop
	}
	switch env.Status {
	case registry.EnvironmentStopped:
		return nil
	case registry.EnvironmentRunning, registry.EnvironmentStarting:
	default:
		return &BadStateError{Op: "stop", Status: env.Status}
	}
	if err := m.refuseIfActive(ctx, targetID); err != nil {
		return err
	}
	if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsStop, protocol.IDParams{ID: env.ID}, nil); err != nil {
		return m.mapPluginErr(err)
	}
	m.cancelJob(env.ID)
	return m.setStatus(ctx, env, registry.EnvironmentStopped, "")
}

// Start brings a stopped machine back at the same address.
func (m *Manager) Start(ctx context.Context, targetID string) error {
	env, _, err := m.managed(ctx, targetID)
	if err != nil {
		return err
	}
	defer m.lock(env.ID)()
	if env, _, err = m.reload(ctx, env.ID); err != nil {
		return err
	}
	switch env.Status {
	case registry.EnvironmentRunning, registry.EnvironmentStarting, registry.EnvironmentCreating, registry.EnvironmentRecreating:
		return nil
	case registry.EnvironmentStopped, registry.EnvironmentLost, registry.EnvironmentError:
	default:
		return &BadStateError{Op: "start", Status: env.Status}
	}
	err = m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsStart, protocol.IDParams{ID: env.ID}, nil)
	if isNotFound(err) || env.Status == registry.EnvironmentLost || env.Status == registry.EnvironmentError {
		// The plugin no longer has it (or never got it): make it again
		// with the same id, key and host key; a persistent machine's
		// data survives where the plugin kept its volume.
		if err := m.setStatus(ctx, env, registry.EnvironmentCreating, ""); err != nil {
			return err
		}
		m.startJob(env.ID, func(ctx context.Context) { m.runCreate(ctx, env.ID) })
		return nil
	}
	if err != nil {
		return m.mapPluginErr(err)
	}
	if err := m.setStatus(ctx, env, registry.EnvironmentStarting, ""); err != nil {
		return err
	}
	m.startJob(env.ID, func(ctx context.Context) { m.follow(ctx, env.ID) })
	return nil
}

// Recreate replaces the running machine with the plugin's configured
// image (and the given size, if any), keeping its data and address.
func (m *Manager) Recreate(ctx context.Context, targetID, size string) error {
	env, target, err := m.managed(ctx, targetID)
	if err != nil {
		return err
	}
	info, err := m.Info(ctx, env.PluginID)
	if err != nil {
		return err
	}
	if size != "" && !hasSize(info, size) {
		return &InvalidError{Field: "size", Msg: "must be one of " + sizeNames(info)}
	}
	defer m.lock(env.ID)()
	if env, target, err = m.reload(ctx, env.ID); err != nil {
		return err
	}
	switch env.Status {
	case registry.EnvironmentRunning, registry.EnvironmentStopped, registry.EnvironmentError:
	default:
		return &BadStateError{Op: "recreate", Status: env.Status}
	}
	if err := m.refuseIfActive(ctx, targetID); err != nil {
		return err
	}
	if size != "" {
		env.Size = size
	}
	spec, err := m.buildSpec(ctx, env, target, info.Image)
	if err != nil {
		return err
	}
	var pe protocol.Environment
	if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsRecreate, protocol.RecreateParams{ID: env.ID, Spec: spec}, &pe); err != nil {
		return m.mapPluginErr(err)
	}
	env.Image, env.Status, env.StatusReason = info.Image, registry.EnvironmentRecreating, ""
	if err := m.store.UpdateEnvironment(ctx, env); err != nil {
		return err
	}
	m.updateGauge(ctx)
	m.startJob(env.ID, func(ctx context.Context) { m.follow(ctx, env.ID) })
	return nil
}

// DeleteTarget is the cascade of design §5 for a machine: refused while
// a task runs there; otherwise its workspaces with their tasks, its
// target-scoped credentials, the machine (data included) through the
// plugin, the generated key, and the rows.
func (m *Manager) DeleteTarget(ctx context.Context, targetID string) error {
	env, err := m.Get(ctx, targetID)
	if err != nil {
		return err
	}
	target, err := m.store.GetTarget(ctx, targetID)
	if err != nil {
		return err
	}
	defer m.lock(env.ID)()
	if env, target, err = m.load(ctx, env.ID); err != nil {
		return err
	}
	if err := m.refuseIfActive(ctx, targetID); err != nil {
		return err
	}
	detached := env.Status == registry.EnvironmentDetached || env.PluginID == ""
	if !detached {
		if _, err := m.requireRunning(ctx, env.PluginID); err != nil {
			return err
		}
	}
	m.cancelJob(env.ID)
	workspaces, err := m.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, ws := range workspaces {
		if ws.TargetID != targetID {
			continue
		}
		if err := m.store.DeleteWorkspaceAndTasks(ctx, ws.ID); err != nil {
			return fmt.Errorf("workspace %s: %w", ws.Name, err)
		}
	}
	creds, err := m.store.ListCredentialInfo(ctx)
	if err != nil {
		return err
	}
	for _, c := range creds {
		if c.TargetID == targetID {
			if err := m.store.DeleteCredential(ctx, c.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
		}
	}
	if !detached {
		env.Status, env.StatusReason = registry.EnvironmentDestroying, ""
		if err := m.store.UpdateEnvironment(ctx, env); err != nil {
			return err
		}
		if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsDestroy, protocol.IDParams{ID: env.ID}, nil); err != nil && !isNotFound(err) {
			return m.mapPluginErr(err)
		}
	}
	if err := m.store.DeleteEnvironment(ctx, env.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if err := m.store.DeleteTarget(ctx, targetID); err != nil {
		return err
	}
	m.releaseTargetKey(ctx, target.SSHKeyRef)
	m.logger.Info("machine destroyed", "target", target.Name, "environment", env.ID)
	m.updateGauge(ctx)
	return nil
}

// releaseTargetKey deletes the target's key if it was made for it.
func (m *Manager) releaseTargetKey(ctx context.Context, keyID string) {
	if keyID == "" {
		return
	}
	keys, err := m.store.ListSSHKeys(ctx)
	if err != nil {
		return
	}
	for _, k := range keys {
		if k.ID == keyID && k.Origin == registry.SSHKeyOriginTarget {
			m.releaseKey(ctx, keyID)
			return
		}
	}
}

// AttachCommands asks the plugin how a person attaches to session on
// the machine, validated.
func (m *Manager) AttachCommands(ctx context.Context, targetID, session string) ([]protocol.AttachCommand, error) {
	env, _, err := m.managed(ctx, targetID)
	if err != nil {
		return nil, err
	}
	var res protocol.AttachResult
	if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsAttachCommands, protocol.AttachParams{ID: env.ID, Session: session}, &res); err != nil {
		var missing *plugins.CapabilityMissingError
		if errors.As(err, &missing) {
			return nil, nil
		}
		return nil, m.mapPluginErr(err)
	}
	out := make([]protocol.AttachCommand, 0, len(res.Commands))
	for _, c := range res.Commands {
		if c.Via == "" || c.Command == "" || len(c.Command) > 512 || strings.ContainsAny(c.Command, "\r\n\x00") {
			continue
		}
		out = append(out, protocol.AttachCommand{Via: truncate(c.Via, 32), Command: c.Command})
	}
	return out, nil
}

// EnsureRunning starts a stopped machine and waits for it to be
// running; a machine already running returns at once. For dispatch.
func (m *Manager) EnsureRunning(ctx context.Context, targetID string) error {
	env, err := m.Get(ctx, targetID)
	if err != nil {
		return err
	}
	switch env.Status {
	case registry.EnvironmentRunning:
		return nil
	case registry.EnvironmentStopped:
		if err := m.Start(ctx, targetID); err != nil {
			return err
		}
	case registry.EnvironmentCreating, registry.EnvironmentStarting, registry.EnvironmentRecreating:
	default:
		return &BadStateError{Op: "use", Status: env.Status}
	}
	deadline := time.After(readyTimeout)
	ticker := time.NewTicker(m.PollInterval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("providers: the machine didn't become ready within %s", readyTimeout)
		case <-ticker.C:
		}
		env, err := m.Get(ctx, targetID)
		if err != nil {
			return err
		}
		switch env.Status {
		case registry.EnvironmentRunning:
			return nil
		case registry.EnvironmentError, registry.EnvironmentLost, registry.EnvironmentDetached, registry.EnvironmentStopped:
			return &BadStateError{Op: "use", Status: env.Status}
		}
	}
}

// MachineCount is how many machines plugin pluginID owns, for the plugin
// manager.
func (m *Manager) MachineCount(ctx context.Context, pluginID string) (int, error) {
	envs, err := m.store.ListEnvironmentsByPlugin(ctx, pluginID)
	if err != nil {
		return 0, err
	}
	return len(envs), nil
}

// UninstallMachines deals with a plugin's machines before it is
// uninstalled: destroy each (the cascade), or keep them as plain hosts
// (detached: the row keeps the history, nothing manages the machine).
func (m *Manager) UninstallMachines(ctx context.Context, pluginID, mode string) error {
	envs, err := m.store.ListEnvironmentsByPlugin(ctx, pluginID)
	if err != nil {
		return err
	}
	for _, env := range envs {
		switch mode {
		case plugins.UninstallDestroy:
			if err := m.DeleteTarget(ctx, env.TargetID); err != nil {
				return fmt.Errorf("machine %s: %w", env.ID, err)
			}
		case plugins.UninstallKeep:
			release := m.lock(env.ID)
			m.cancelJob(env.ID)
			cur, err := m.store.GetEnvironment(ctx, env.ID)
			if err == nil {
				cur.PluginID, cur.Status, cur.StatusReason = "", registry.EnvironmentDetached, "the plugin was uninstalled keeping its machines"
				err = m.store.UpdateEnvironment(ctx, cur)
			}
			release()
			if err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
		default:
			return plugins.ErrBadUninstallMode
		}
	}
	m.updateGauge(ctx)
	return nil
}

func (m *Manager) setStatus(ctx context.Context, env *registry.Environment, status registry.EnvironmentStatus, reason string) error {
	env.Status, env.StatusReason = status, reason
	if err := m.store.UpdateEnvironment(ctx, env); err != nil {
		return err
	}
	m.updateGauge(ctx)
	return nil
}

func (m *Manager) cancelJob(envID string) {
	m.mu.Lock()
	if j := m.running[envID]; j != nil {
		j.cancel()
		delete(m.running, envID)
	}
	m.mu.Unlock()
}

// HandleNotify is the plugin manager's OnNotify: a targets.changed hint
// reconciles that machine soon.
func (m *Manager) HandleNotify(pluginID, method string, params json.RawMessage) {
	if method != protocol.NotifyTargetsChanged {
		return
	}
	var tc protocol.TargetsChanged
	if err := json.Unmarshal(params, &tc); err != nil || tc.ID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if env, err := m.store.GetEnvironment(ctx, tc.ID); err == nil && env.PluginID == pluginID {
			m.reconcileOne(ctx, env)
		}
	}()
}

// updateGauge recomputes loomux_environments from the rows.
func (m *Manager) updateGauge(ctx context.Context) {
	envs, err := m.store.ListEnvironments(ctx)
	if err != nil {
		return
	}
	m.met.EnvironmentsByStatus.Reset()
	counts := map[[2]string]int{}
	for _, e := range envs {
		counts[[2]string{e.PluginName, string(e.Status)}]++
	}
	for k, n := range counts {
		m.met.EnvironmentsByStatus.WithLabelValues(k[0], k[1]).Set(float64(n))
	}
}

// Close cancels the background jobs and waits for them.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for id, j := range m.running {
		j.cancel()
		delete(m.running, id)
	}
	m.mu.Unlock()
	m.jobs.Wait()
}

// MachineView is what the API shows of a machine: its environment, the
// plugin that owns it, whether the plugin's configured image differs
// from what runs, and the plugin's current warnings.
type MachineView struct {
	Environment     registry.Environment
	PluginLabel     string
	UpdateAvailable bool
	Warnings        []protocol.Problem
}

// View is the machine behind targetID; ErrNotMachine for a registered
// host.
func (m *Manager) View(ctx context.Context, targetID string) (*MachineView, error) {
	env, err := m.Get(ctx, targetID)
	if err != nil {
		return nil, err
	}
	views, err := m.pluginViews(ctx)
	if err != nil {
		return nil, err
	}
	return m.view(ctx, env, views), nil
}

// Views is every machine, by target id, for the targets list.
func (m *Manager) Views(ctx context.Context) (map[string]*MachineView, error) {
	envs, err := m.store.ListEnvironments(ctx)
	if err != nil {
		return nil, err
	}
	views, err := m.pluginViews(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*MachineView, len(envs))
	for _, env := range envs {
		out[env.TargetID] = m.view(ctx, env, views)
	}
	return out, nil
}

func (m *Manager) pluginViews(ctx context.Context) (map[string]*plugins.View, error) {
	list, err := m.plugins.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*plugins.View, len(list))
	for _, v := range list {
		out[v.ID] = v
	}
	return out, nil
}

func (m *Manager) view(ctx context.Context, env *registry.Environment, views map[string]*plugins.View) *MachineView {
	mv := &MachineView{Environment: *env}
	mv.Environment.HostPrivateKey = nil
	v := views[env.PluginID]
	if v == nil {
		return mv
	}
	mv.PluginLabel = v.Label
	if v.Check != nil {
		for _, p := range v.Check.Problems {
			if p.Severity == protocol.SeverityWarning {
				mv.Warnings = append(mv.Warnings, p)
			}
		}
	}
	if v.Instance.State == plugins.StateRunning && (env.Status == registry.EnvironmentRunning || env.Status == registry.EnvironmentStopped) {
		if info, err := m.Info(ctx, env.PluginID); err == nil && info.Image != "" && info.Image != env.Image {
			mv.UpdateAvailable = true
		}
	}
	return mv
}
