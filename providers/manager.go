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
	ctx    context.Context
	// progress is what the plugin last said the machine is doing, as the
	// job's poll saw it: protocol.ProgressImagePull, or "" (LOOM-222).
	progress string
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
	// DefaultPullTimeout bounds how far an image pull in progress pushes
	// a job's deadline back (LOOM-203): a cold pull of the 542 MB agent
	// image over a slow link outlasted CreateTimeout on TEST.
	DefaultPullTimeout  = 30 * time.Minute
	DefaultPollInterval = 2 * time.Second
	DefaultOrphanTTL    = 24 * time.Hour
	// DefaultReadyTimeout bounds EnsureRunning's wait for a machine to
	// come up.
	DefaultReadyTimeout = 5 * time.Minute
	// DefaultOfferTimeout bounds each instance's describe in Offers.
	DefaultOfferTimeout = 3 * time.Second
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
	// TmuxSocket is the instance's tmux socket (its executors'
	// ExecutorOptions.TmuxSocket), the one a machine's sessions run on:
	// the plugin's attach commands name it (LOOM-210). Empty is
	// targets.DefaultTmuxSocket.
	TmuxSocket string
	// IdleStop, when set, stops a persistent machine idle for that long
	// at each reconcile, and Activity says what is happening on its
	// target (idle.go); zero, or a nil Activity, never stops one.
	IdleStop time.Duration
	Activity Activity

	CreateTimeout time.Duration
	// ReadyTimeout bounds a dispatch's wait (EnsureRunning) for a machine
	// to come up; the time its image is pulling doesn't count, up to
	// PullTimeout (LOOM-222).
	ReadyTimeout time.Duration
	// PullTimeout is how much longer than CreateTimeout a job waits
	// while the plugin reports the machine's image being pulled
	// (progress image_pull): the time it does doesn't count against
	// CreateTimeout, up to this much (LOOM-203).
	PullTimeout time.Duration
	// OfferTimeout bounds each instance's describe in Offers, which a
	// chat message waits on (LOOM-216): an instance slower than that is
	// left out of the offers rather than holding up the chat.
	OfferTimeout time.Duration
	PollInterval time.Duration
	OrphanTTL    time.Duration
	// NotifyDelay is how long a machine's targets.changed waits before
	// its reconcile, so a burst of them is one (LOOM-213).
	NotifyDelay time.Duration
	Now         func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// jobs counts every goroutine the manager starts — machine jobs and
	// notifies (LOOM-196) — which Close waits for.
	jobs sync.WaitGroup
	// life is cancelled by Close: a notify in flight then stops rather
	// than outlive the store.
	life    context.Context
	endLife context.CancelFunc
	// running is the background job following each machine, if any.
	running map[string]*job
	// orphanSeen is when an orphan was first listed, keyed plugin/env.
	orphanSeen map[string]time.Time
	// notified is each machine a targets.changed reconcile is pending or
	// running for, keyed plugin/env (HandleNotify).
	notified map[string]*notifyRun
	// lastUse is when each running machine was last started or handed
	// to a dispatch, as this process saw it (idle.go).
	lastUse map[string]time.Time
	closed  bool
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
	life, endLife := context.WithCancel(context.Background())
	return &Manager{
		store: store, plugins: pm, logger: logger, met: met, instanceID: instanceID,
		CreateTimeout: DefaultCreateTimeout, ReadyTimeout: DefaultReadyTimeout, PullTimeout: DefaultPullTimeout, PollInterval: DefaultPollInterval, OrphanTTL: DefaultOrphanTTL,
		NotifyDelay: DefaultNotifyDelay, OfferTimeout: DefaultOfferTimeout,
		Now:   time.Now,
		locks: map[string]*sync.Mutex{}, running: map[string]*job{}, orphanSeen: map[string]time.Time{}, lastUse: map[string]time.Time{},
		notified: map[string]*notifyRun{},
		life:     life, endLife: endLife,
	}
}

func (m *Manager) tmuxSocket() string {
	if m.TmuxSocket == "" {
		return targets.DefaultTmuxSocket
	}
	return m.TmuxSocket
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
	// NamedTrust makes egress trusted wait for the plugin to say, for
	// this target's name, that it offers it (LOOM-216): a machine the
	// router proposes is never trusted on the plugin's name-independent
	// describe alone. A plugin that can't answer for a name refuses it.
	NamedTrust bool
	// Expect, when set, is the plan Create must make (LOOM-216): what a
	// chat offer showed. If the request resolves to anything else by the
	// time Create plans it, nothing is made and Create returns
	// ErrPlanChanged. The counts aren't compared.
	Expect *Plan
}

// Plan is what Create would make for a request, every default resolved
// (LOOM-216): what a chat offer shows before anything is created.
type Plan struct {
	PluginID    string
	PluginName  string
	PluginLabel string
	Name        string
	// Size is the plugin's size of that name, as it describes it now.
	Size       protocol.Size
	Persistent bool
	Egress     string
	// Machines and MaxMachines are the plugin's count and limit (0: none).
	Machines    int
	MaxMachines int
}

// Info is what the plugin offers (targets.describe).
func (m *Manager) Info(ctx context.Context, pluginID string) (protocol.TargetsInfo, error) {
	var info protocol.TargetsInfo
	if err := m.plugins.Call(ctx, pluginID, protocol.MethodTargetsDescribe, nil, &info); err != nil {
		return info, m.mapPluginErr(err)
	}
	return info, nil
}

// Describe is what plugin instance pluginID offers for a new machine
// (its targets.describe), for the create form (LOOM-238), bounded as
// any plugin output is: at most maxOffered sizes and egress options,
// their strings truncated. registry.ErrNotFound for no such instance,
// ErrPluginUnavailable when it isn't installed, enabled and running,
// *plugins.CapabilityMissingError when it doesn't make machines.
func (m *Manager) Describe(ctx context.Context, pluginID string) (protocol.TargetsInfo, error) {
	v, err := m.plugins.Get(ctx, pluginID)
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	if v.Status != registry.PluginStatusInstalled || !v.Enabled || v.Instance.State != plugins.StateRunning {
		return protocol.TargetsInfo{}, ErrPluginUnavailable
	}
	if !contains(v.Capabilities, protocol.CapTargetsCreate) {
		return protocol.TargetsInfo{}, &plugins.CapabilityMissingError{Method: protocol.MethodTargetsDescribe, Capability: protocol.CapTargetsCreate}
	}
	info, err := m.Info(ctx, pluginID)
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	out := protocol.TargetsInfo{
		Sizes:             []protocol.Size{},
		PersistentDefault: info.PersistentDefault,
		EgressOptions:     []string{},
		Image:             truncate(info.Image, 512),
		MaxEnvironments:   max(info.MaxEnvironments, 0),
		Environments:      max(info.Environments, 0),
	}
	for _, s := range info.Sizes[:min(len(info.Sizes), maxOffered)] {
		out.Sizes = append(out.Sizes, boundSize(s))
	}
	for _, e := range info.EgressOptions[:min(len(info.EgressOptions), maxOffered)] {
		out.EgressOptions = append(out.EgressOptions, truncate(e, 32))
	}
	return out, nil
}

// maxOffered bounds the sizes and egress options Describe passes on.
const maxOffered = 32

// boundSize is s with its strings truncated, requests included (#450).
func boundSize(s protocol.Size) protocol.Size {
	return protocol.Size{Name: truncate(s.Name, 64), CPU: truncate(s.CPU, 32), Memory: truncate(s.Memory, 32), Disk: truncate(s.Disk, 32),
		CPURequest: truncate(s.CPURequest, 32), MemoryRequest: truncate(s.MemoryRequest, 32)}
}

// Offer is one plugin instance that makes machines, with what it offers
// now (LOOM-216): the routing model's view of where a machine can be
// created.
type Offer struct {
	PluginID    string
	PluginName  string
	PluginLabel string
	Info        protocol.TargetsInfo
	// Persistent and Ephemeral say which kinds of machine it makes.
	Persistent bool
	Ephemeral  bool
}

// Offers lists the plugin instances that can make a machine now:
// installed, enabled, running and declaring targets.create, each with
// its Describe. An instance whose describe fails, or takes longer than
// OfferTimeout, is left out.
func (m *Manager) Offers(ctx context.Context) ([]Offer, error) {
	views, err := m.plugins.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Offer
	for _, v := range views {
		if v.Status != registry.PluginStatusInstalled || !v.Enabled || v.Instance.State != plugins.StateRunning ||
			!contains(v.Capabilities, protocol.CapTargetsCreate) {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, m.OfferTimeout)
		info, err := m.Describe(dctx, v.ID)
		cancel()
		if err != nil {
			m.logger.Debug("plugin left out of the machine offers", "plugin", v.ID, "error", err)
			continue
		}
		out = append(out, Offer{PluginID: v.ID, PluginName: v.Name, PluginLabel: v.Label, Info: info,
			Persistent: contains(v.Capabilities, protocol.CapTargetsPersistent),
			Ephemeral:  contains(v.Capabilities, protocol.CapTargetsEphemeral)})
	}
	return out, nil
}

// trustedFor is nil when the plugin offers egress trusted to a machine
// named name, and otherwise the *InvalidError saying why not
// (trustRefusal), from its describe asked for that name.
func (m *Manager) trustedFor(ctx context.Context, pluginID, name string) error {
	var info protocol.TargetsInfo
	if err := m.plugins.Call(ctx, pluginID, protocol.MethodTargetsDescribe, protocol.DescribeParams{Name: name}, &info); err != nil {
		return m.mapPluginErr(err)
	}
	return trustRefusal(name, info)
}

// trustRefusal is nil when info, a describe asked for name, answered for
// it (for_name) with trusted among the options, and otherwise why
// trusted is refused. A plugin that doesn't answer for a name (one older
// than LOOM-216) is a no, saying so: its trusted_machines may well list
// name.
func trustRefusal(name string, info protocol.TargetsInfo) error {
	if info.ForName == "" {
		return &InvalidError{Field: "egress", Msg: "trusted is only for the machines the plugin's trusted_machines names, " +
			"and this plugin is too old to say which names those are; update it, or create the machine " +
			"on the Targets page or with POST /api/v1/targets"}
	}
	if info.ForName == name && contains(info.EgressOptions, protocol.EgressTrusted) {
		return nil
	}
	return &InvalidError{Field: "egress", Msg: "trusted is only for the machines the plugin's trusted_machines names, and " +
		strconv.Quote(name) + " isn't one"}
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

// Plan resolves req as Create would, without creating anything: the
// size, persistence and egress it would use, or why it would refuse.
func (m *Manager) Plan(ctx context.Context, req CreateRequest) (*Plan, error) {
	p, _, _, err := m.plan(ctx, req)
	return p, err
}

func (m *Manager) plan(ctx context.Context, req CreateRequest) (*Plan, *plugins.View, protocol.TargetsInfo, error) {
	var info protocol.TargetsInfo
	view, err := m.requireRunning(ctx, req.PluginID)
	if err != nil {
		return nil, nil, info, err
	}
	if info, err = m.Info(ctx, req.PluginID); err != nil {
		return nil, nil, info, err
	}
	caps := map[string]bool{}
	for _, c := range view.Capabilities {
		caps[c] = true
	}
	if !caps[protocol.CapTargetsCreate] {
		return nil, nil, info, &plugins.CapabilityMissingError{Method: protocol.MethodTargetsCreate, Capability: protocol.CapTargetsCreate}
	}
	size := req.Size
	if size == "" && len(info.Sizes) > 0 {
		size = info.Sizes[0].Name
	}
	if !hasSize(info, size) {
		return nil, nil, info, &InvalidError{Field: "size", Msg: "must be one of " + sizeNames(info)}
	}
	persistent := info.PersistentDefault
	if req.Persistent != nil {
		persistent = *req.Persistent
	}
	if persistent && !caps[protocol.CapTargetsPersistent] && caps[protocol.CapTargetsEphemeral] {
		return nil, nil, info, &InvalidError{Field: "persistent", Msg: "this plugin only makes ephemeral machines"}
	}
	if !persistent && !caps[protocol.CapTargetsEphemeral] {
		return nil, nil, info, &InvalidError{Field: "persistent", Msg: "this plugin only makes machines that keep their data"}
	}
	egress, err := chooseEgress(info.EgressOptions, req.Egress)
	if err != nil {
		return nil, nil, info, err
	}
	if egress == protocol.EgressTrusted && req.NamedTrust {
		if err := m.trustedFor(ctx, req.PluginID, strings.TrimSpace(req.Target.Name)); err != nil {
			return nil, nil, info, err
		}
	}
	machines := 0
	if info.MaxEnvironments > 0 {
		existing, err := m.store.ListEnvironmentsByPlugin(ctx, req.PluginID)
		if err != nil {
			return nil, nil, info, err
		}
		if machines = len(existing); machines >= info.MaxEnvironments {
			return nil, nil, info, fmt.Errorf("%w (%d)", ErrQuota, info.MaxEnvironments)
		}
	}
	if info.Port < 1 || info.Port > 65535 || info.User == "" {
		return nil, nil, info, &BadAddressError{Detail: "the plugin's describe gives no usable sshd user and port"}
	}
	var sz protocol.Size
	for _, s := range info.Sizes {
		if s.Name == size {
			sz = s
			break
		}
	}
	return &Plan{PluginID: req.PluginID, PluginName: view.Name, PluginLabel: view.Label, Name: strings.TrimSpace(req.Target.Name),
		Size: sz, Persistent: persistent, Egress: egress, Machines: machines, MaxMachines: info.MaxEnvironments}, view, info, nil
}

// Create registers the target and the environment (rows first), then
// asks the plugin for the machine in the background. It returns once the
// rows exist; the target becomes ready as the machine comes up.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*registry.Target, *registry.Environment, error) {
	plan, view, info, err := m.plan(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if req.Expect != nil && !samePlan(*plan, *req.Expect) {
		return nil, nil, ErrPlanChanged
	}
	size, persistent, egress := plan.Size.Name, plan.Persistent, plan.Egress

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
	if egress == protocol.EgressTrusted {
		// The record of an opt-in that opens the LAN and the tailnet
		// (LOOM-226): the row keeps egress, this says when and to whom.
		m.logger.Warn("machine created with egress trusted: it may reach the LAN and the tailnet",
			"target", target.Name, "target_id", target.ID, "environment", env.ID, "plugin", req.PluginID)
	}
	m.updateGauge(ctx)
	m.startJob(env.ID, func(ctx context.Context) { m.runCreate(ctx, env.ID) })
	return &target, env, nil
}

// samePlan reports whether a and b make the same machine; the plugin's
// counts are not part of it.
func samePlan(a, b Plan) bool {
	a.Machines, a.MaxMachines, b.Machines, b.MaxMachines = 0, 0, 0, 0
	return a == b
}

// chooseEgress is the machine's egress: the one asked for, if offered;
// unasked, the plugin's first option that isn't trusted (LOOM-226:
// never the default), or internet when it offers no choice.
func chooseEgress(options []string, asked string) (string, error) {
	if len(options) == 0 {
		if asked != "" {
			return "", &InvalidError{Field: "egress", Msg: "this plugin offers no egress choice right now"}
		}
		return protocol.EgressInternet, nil
	}
	if asked == "" {
		for _, o := range options {
			if o != protocol.EgressTrusted {
				return o, nil
			}
		}
		return "", &InvalidError{Field: "egress", Msg: "egress trusted is never the default; ask for it"}
	}
	if !contains(options, asked) {
		return "", &InvalidError{Field: "egress", Msg: "must be one of " + strings.Join(options, ", ")}
	}
	return asked, nil
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

// startJob runs fn on its own goroutine, bounded by CreateTimeout (and
// whatever an image pull adds, follow), and replacing any job for the
// same environment.
func (m *Manager) startJob(envID string, fn func(ctx context.Context)) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if old := m.running[envID]; old != nil {
		old.cancel()
	}
	parent, cancel := context.WithCancel(context.Background())
	ctx := newJobContext(parent, m.CreateTimeout)
	j := &job{cancel: cancel, ctx: ctx}
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

// setProgress records progress as what the job running under ctx last
// saw of the machine; a job replaced since records nothing.
func (m *Manager) setProgress(ctx context.Context, envID, progress string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.running[envID]; j != nil && j.ctx == ctx {
		j.progress = progress
	}
}

// progress is what the job following the machine last saw it doing:
// protocol.ProgressImagePull, or "" (none, or no job).
func (m *Manager) progress(envID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.running[envID]; j != nil {
		return j.progress
	}
	return ""
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			m.failJob(ctx, envID, m.deadlineReason(ctx))
		}
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
// While the plugin reports the machine's image being pulled (progress
// image_pull) the deadline stands still: what was left of it when the
// pull was first seen stays left, plus a poll's grace, up to PullTimeout
// past the deadline as made. So a slow first pull isn't a failure,
// while a machine stuck any other way, or a pull that never ends, still
// is (LOOM-203).
func (m *Manager) follow(ctx context.Context, envID string) {
	ticker := time.NewTicker(m.PollInterval)
	defer ticker.Stop()
	jc, _ := ctx.(*jobContext)
	var left time.Duration // when the pull was first seen; 0: not pulling
	for {
		env, _, err := m.load(ctx, envID)
		if err != nil {
			// A read the deadline cut short is the deadline: without
			// this the machine was left starting for ever.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				m.failJob(ctx, envID, m.deadlineReason(ctx))
			}
			return
		}
		if !inFlight(env.Status) {
			return
		}
		var pe protocol.Environment
		err = m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: env.ID}, &pe)
		if err == nil {
			m.setProgress(ctx, envID, pe.Progress)
		} else {
			m.setProgress(ctx, envID, "")
		}
		if jc != nil && err == nil && pe.Progress == protocol.ProgressImagePull {
			if left <= 0 {
				left = jc.remaining()
			}
			if left > 0 {
				jc.extendTo(time.Now().Add(left+m.PollInterval), m.PullTimeout)
			}
		} else {
			left = 0
		}
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
				m.failJob(ctx, envID, m.deadlineReason(ctx))
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
	m.noteRunning(env.ID, status == registry.EnvironmentRunning)
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
// Once the job's deadline has passed, the deadline is the reason: a
// step that failed because it hit the deadline mid-call (a store write)
// would otherwise name its own error.
func (m *Manager) failJob(ctx context.Context, envID, reason string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = m.deadlineReason(ctx)
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
	return m.stop(ctx, targetID, "", nil)
}

// stop is Stop recording reason, and only while still (when set) holds
// for the machine as re-read under its lock (errNotIdle otherwise).
func (m *Manager) stop(ctx context.Context, targetID, reason string, still func(*registry.Environment) bool) error {
	env, _, err := m.managed(ctx, targetID)
	if err != nil {
		return err
	}
	defer m.lock(env.ID)()
	if env, _, err = m.reload(ctx, env.ID); err != nil {
		return err
	}
	if still != nil && !still(env) {
		return errNotIdle
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
	return m.setStatus(ctx, env, registry.EnvironmentStopped, reason)
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
// the machine, on the instance's tmux socket, validated.
func (m *Manager) AttachCommands(ctx context.Context, targetID, session string) ([]protocol.AttachCommand, error) {
	env, _, err := m.managed(ctx, targetID)
	if err != nil {
		return nil, err
	}
	var res protocol.AttachResult
	if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsAttachCommands, protocol.AttachParams{ID: env.ID, Session: session, Socket: m.tmuxSocket()}, &res); err != nil {
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
// The wait is ReadyTimeout, which stands still while the job following
// the machine sees its image pulling, up to PullTimeout more: a cold
// pull isn't a machine that won't come up (LOOM-222).
func (m *Manager) EnsureRunning(ctx context.Context, targetID string) (err error) {
	env, err := m.Get(ctx, targetID)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			m.markUsed(env.ID) // in use from now: not idle (idle.go)
		}
	}()
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
	envID := env.ID
	last := time.Now()
	deadline := last.Add(m.ReadyTimeout)
	latest := deadline.Add(m.PullTimeout)
	var pulled time.Duration
	ticker := time.NewTicker(m.PollInterval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		now := time.Now()
		if m.progress(envID) == protocol.ProgressImagePull {
			step := now.Sub(last)
			pulled += step
			if deadline = deadline.Add(step); deadline.After(latest) {
				deadline = latest
			}
		}
		last = now
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
		if now.After(deadline) {
			return m.readyTimeoutError(pulled)
		}
	}
}

// readyTimeoutError is EnsureRunning's error when its wait ends: the
// wait, and what an image pull added to it.
func (m *Manager) readyTimeoutError(pulled time.Duration) error {
	reason := "providers: the machine didn't become ready within " + m.ReadyTimeout.String()
	switch {
	case pulled >= m.PullTimeout:
		reason += ", besides " + m.PullTimeout.String() + " of pulling its image, which hadn't finished"
	case pulled > 0:
		reason += ", besides " + pulled.Round(time.Second).String() + " of pulling its image"
	}
	return errors.New(reason)
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
	m.noteRunning(env.ID, status == registry.EnvironmentRunning)
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

// DefaultNotifyDelay is Manager.NotifyDelay's default.
const DefaultNotifyDelay = 200 * time.Millisecond

// maxNotified bounds the machines with a targets.changed reconcile
// pending at once: a plugin naming ids at random can't grow it further.
const maxNotified = 1024

// notifyRun is one machine's targets.changed reconcile: running once it
// has started, again when another came in meanwhile.
type notifyRun struct{ running, again bool }

// HandleNotify is the plugin manager's OnNotify: a targets.changed hint
// reconciles that machine (the plugin's own) NotifyDelay on, as the
// periodic reconcile would (LOOM-213). Hints for one machine coalesce:
// those within the delay are one reconcile, and those that come while
// it runs are one more after it, so a state reached mid-reconcile isn't
// missed. Any other notification is ignored, as a peer must.
func (m *Manager) HandleNotify(pluginID, method string, params json.RawMessage) {
	if method != protocol.NotifyTargetsChanged {
		return
	}
	var tc protocol.TargetsChanged
	if err := json.Unmarshal(params, &tc); err != nil || tc.ID == "" {
		return
	}
	key := pluginID + "/" + tc.ID
	// Tracked like a job (LOOM-196): Close cancels it and waits for it,
	// so it never uses the store after the app has closed it.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if run := m.notified[key]; run != nil {
		if run.running {
			run.again = true
		}
		return
	}
	if len(m.notified) >= maxNotified {
		return
	}
	run := &notifyRun{}
	m.notified[key] = run
	m.jobs.Add(1)
	go m.notifyReconcile(pluginID, tc.ID, key, run)
}

// notifyReconcile is HandleNotify's reconcile of one machine.
func (m *Manager) notifyReconcile(pluginID, envID, key string, run *notifyRun) {
	defer m.jobs.Done()
	for {
		select {
		case <-m.life.Done():
			return
		case <-time.After(m.NotifyDelay):
		}
		m.mu.Lock()
		run.running = true
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(m.life, time.Minute)
		if env, err := m.store.GetEnvironment(ctx, envID); err == nil && env.PluginID == pluginID {
			m.reconcileOne(ctx, env)
		}
		cancel()
		m.mu.Lock()
		if !run.again {
			delete(m.notified, key)
			m.mu.Unlock()
			return
		}
		run.running, run.again = false, false
		m.mu.Unlock()
	}
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

// Close cancels the background jobs and notifies in flight, and waits
// for them; a notify after that starts nothing.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for id, j := range m.running {
		j.cancel()
		delete(m.running, id)
	}
	m.mu.Unlock()
	m.endLife()
	m.jobs.Wait()
}

// MachineView is what the API shows of a machine: its environment, the
// plugin that owns it, whether the plugin's configured image differs
// from what runs, the plugin's current warnings, and what the machine
// is doing while it comes up.
type MachineView struct {
	Environment     registry.Environment
	PluginLabel     string
	UpdateAvailable bool
	Warnings        []protocol.Problem
	// Progress is what the plugin last said a machine coming up is
	// doing, as the job following it saw it: protocol.ProgressImagePull,
	// or "" (LOOM-239).
	Progress string
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
	if inFlight(env.Status) {
		mv.Progress = m.progress(env.ID)
	}
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

// deadlineReason is the error a job records when its deadline passes:
// CreateTimeout, and what an image pull added to it.
func (m *Manager) deadlineReason(ctx context.Context) string {
	reason := "the machine didn't become ready within " + m.CreateTimeout.String()
	if jc, ok := ctx.(*jobContext); ok {
		switch pulled := jc.extended(); {
		case pulled >= m.PullTimeout:
			reason += ", besides " + m.PullTimeout.String() + " of pulling its image, which hadn't finished"
		case pulled > 0:
			reason += ", besides " + pulled.Round(time.Second).String() + " of pulling its image"
		}
	}
	return reason
}
