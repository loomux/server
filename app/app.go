package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Loomux/server/agents"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/internal/health"
	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/internal/retention"
	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/version"
)

// staleProvisioningAfter is how long a workspace may stay provisioning
// before the reaper fails it as abandoned (LOOM-60): the router's own
// provisioning bound plus slack for the probe and session launch that
// precede it, so a live provisioning is always failed (or finished) by
// the router first.
const staleProvisioningAfter = router.ProvisionTimeout + 5*time.Minute

// App is the fully wired Loomux domain layer, exposed through the one
// operation this package's "minimal internal interface" needs —
// Dispatch — plus lifecycle cleanup and read access to the underlying
// Store for a client-facing layer (api.Server) that needs its own
// session storage in the same database.
type App struct {
	router     *router.Router
	agentTypes []string
	// cancelTaskDirect cancels a task no dispatch is driving; the router's
	// CancelTask, a seam for tests.
	cancelTaskDirect func(ctx context.Context, taskID string) error
	orch             *orchestrator.Orchestrator
	store            *sqlite.Store
	stopReaper       context.CancelFunc
	reaperDone       chan struct{}
	proberDone       chan struct{}
	sweepDone        chan struct{}
	metrics          *metrics.Metrics
	healthChecker    *health.Checker
	dispatches       *dispatch.Service
	dispatchDrain    time.Duration
	sshAgents        *targets.AgentPool
	routerSettings   *llmrouter.Settings
	plugins          *plugins.Manager
	machines         *providers.Manager
	// bg holds the goroutines Close waits for before closing the store
	// (LOOM-163).
	bg *background
	// bgCtx is the background work's context, cancelled by Close.
	bgCtx context.Context
	// retention and logger configure StartRetention's sweep (LOOM-193).
	retention     retention.Config
	logger        *slog.Logger
	retentionOnce sync.Once
}

// RouterSettings serves /api/v1/settings/router (LOOM-185): the router
// model's tiers as stored through Settings over the environment's,
// applied to the running model without a restart.
func (a *App) RouterSettings() *llmrouter.Settings {
	return a.routerSettings
}

// Dispatches is the dispatch-job service (LOOM-80) the client API
// submits chat messages to: each runs through the router as a job on a
// server-owned context, outliving the request that submitted it.
func (a *App) Dispatches() *dispatch.Service {
	return a.dispatches
}

// DrainDispatches stops accepting dispatch jobs and gives in-flight ones
// the configured drain time to finish; any still running are then marked
// interrupted, their tasks left for startup reconciliation (LOOM-82).
// Call it on shutdown before the HTTP server's own Shutdown, so blocking
// requests waiting on a job get their answer first.
func (a *App) DrainDispatches() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.dispatchDrain)
	defer cancel()
	return a.dispatches.Shutdown(ctx)
}

// Dispatch routes one chat message through the full pipeline. See
// router.Router.Dispatch. workspaceHint (LOOM-46) is an optional,
// advisory workspace ID — empty means no hint — surfaced as a plain
// string rather than a router.DispatchOption specifically so callers
// like api.Server's Dispatcher interface don't need to import the
// router package just to supply it (mirroring api.Dispatcher's own
// narrow-seam rationale).
func (a *App) Dispatch(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
	var opts []router.DispatchOption
	if workspaceHint != "" {
		opts = append(opts, router.WithWorkspaceHint(workspaceHint))
	}
	return a.router.Dispatch(ctx, conversationID, message, opts...)
}

// DeleteWorkspace deletes a workspace, its tasks and their sessions
// (LOOM-70) — see orchestrator.Orchestrator.DeleteWorkspace. Satisfies
// api.WorkspaceManager.
func (a *App) DeleteWorkspace(ctx context.Context, id string) ([]string, error) {
	return a.orch.DeleteWorkspace(ctx, id)
}

// SetWorkspaceStatus reopens (idle) or archives a workspace (LOOM-70) —
// see orchestrator.Orchestrator.ReopenWorkspace and ArchiveWorkspace.
// Satisfies api.WorkspaceManager.
func (a *App) SetWorkspaceStatus(ctx context.Context, id string, status registry.WorkspaceStatus) error {
	switch status {
	case registry.WorkspaceStatusIdle:
		return a.orch.ReopenWorkspace(ctx, id)
	case registry.WorkspaceStatusArchived:
		return a.orch.ArchiveWorkspace(ctx, id)
	}
	return fmt.Errorf("app: a workspace can't be set to %q", status)
}

// CancelTask cancels a task (LOOM-99). A running task is the one its
// conversation's running dispatch drives — only one task is ever mid-turn
// — so it is cancelled through that dispatch, which ends the turn. Any
// other open task (left awaiting input, stopped at a prompt, or running
// with no dispatch after a restart) is cancelled directly — see
// router.Router.CancelTask — never through a dispatch driving some other
// task of the conversation. Satisfies api.TaskCanceller.
func (a *App) CancelTask(ctx context.Context, taskID string) (string, error) {
	task, err := a.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task.Status == registry.TaskStatusCompleted || task.Status == registry.TaskStatusFailed {
		return "", orchestrator.ErrTaskInactive
	}
	if task.Status == registry.TaskStatusRunning && task.ConversationID != "" {
		ds, err := a.dispatches.ListByConversation(ctx, task.ConversationID)
		if err != nil {
			return "", err
		}
		for _, d := range ds {
			if d.Status.Terminal() {
				continue
			}
			if err := a.dispatches.Cancel(ctx, d.ID); err == nil {
				return d.ID, nil
			} else if !errors.Is(err, dispatch.ErrNotRunning) {
				return "", err
			}
			// It ended between the list and the cancel: the task has just
			// finished its turn. The user asked to cancel it, so it is,
			// directly.
		}
	}
	return "", a.cancelTaskDirect(ctx, taskID)
}

// ProbeTarget probes a target's health and agent CLIs now and records the
// results (LOOM-86) — see router.Router.ProbeTarget. Satisfies
// api.TargetProber.
func (a *App) ProbeTarget(ctx context.Context, targetID string) (*registry.TargetHealth, []*registry.TargetAgent, error) {
	return a.router.ProbeTarget(ctx, targetID)
}

// firstProbeAfter caps how long after startup the first target health
// probe runs (LOOM-86): soon, but clear of startup itself.
const firstProbeAfter = 30 * time.Second

// runTargetProber probes every target's health every interval until ctx
// ends, the first time after interval or firstProbeAfter, whichever is
// sooner.
func runTargetProber(ctx context.Context, rtr *router.Router, interval time.Duration) {
	timer := time.NewTimer(min(interval, firstProbeAfter))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		rtr.ProbeAllTargets(ctx)
		timer.Reset(interval)
	}
}

// AgentTypeNames are the agent types this server can launch, sorted
// (LOOM-122: what a target's allowed_agent_types may name).
func (a *App) AgentTypeNames() []string {
	names := append([]string(nil), a.agentTypes...)
	sort.Strings(names)
	return names
}

// Store returns the underlying registry.Store — e.g. for api.Server's
// session storage, which must live in the same database as everything
// else rather than a second, separately managed connection.
func (a *App) Store() registry.Store {
	return a.store
}

// Metrics returns the Prometheus metrics bundle so cmd/loomuxd can
// expose it on a scraper port (LOOM-103).
func (a *App) Metrics() *metrics.Metrics {
	return a.metrics
}

// HealthChecker returns the deep-health checker for api.Server to expose
// on /api/v1/health and /api/v1/health/deep (LOOM-105).
func (a *App) HealthChecker() *health.Checker {
	return a.healthChecker
}

// Plugins serves /api/v1/plugins (LOOM-178): the plugin system's
// catalog and installed instances.
func (a *App) Plugins() *plugins.Manager {
	return a.plugins
}

// Machines serves the machine side of /api/v1/targets (LOOM-178):
// targets plugins made.
func (a *App) Machines() *providers.Manager {
	return a.machines
}

// StartRetention starts the retention sweep (LOOM-193): every
// retentionSweepInterval, and once now, it deletes sessions expired
// longer than Config.SessionRetention, dispatches finished longer than
// Config.DispatchRetention and confirmations resolved longer than
// Config.ConfirmationRetention. Messages are kept. sessionTTL and
// sessionMaxAge are the session lifetime the client API enforces (its
// sliding window and absolute lifetime, 0 for none), which say when a
// session expired. Close stops it; only the first call starts it.
func (a *App) StartRetention(sessionTTL, sessionMaxAge time.Duration) {
	a.retentionOnce.Do(func() {
		cfg := a.retention
		cfg.SessionTTL, cfg.SessionMaxAge = sessionTTL, sessionMaxAge
		sweeper := retention.New(a.store, cfg, retention.WithMetrics(a.metrics), retention.WithLogger(a.logger))
		a.bg.Go(func() { sweeper.Run(a.bgCtx, retentionSweepInterval) })
	})
}

// Close stops the background idle reaper (LOOM-16) and releases the
// store's resources (its DB connection).
func (a *App) Close() error {
	if a.dispatches != nil {
		// Normally already drained (DrainDispatches); otherwise interrupt
		// whatever is still running rather than wait for it.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = a.dispatches.Shutdown(ctx)
	}
	if a.stopReaper != nil {
		a.stopReaper()
		<-a.reaperDone
		<-a.proberDone
		<-a.sweepDone
	}
	// Reconciliation and the sweepers stop with the reaper's context;
	// a notification on its way is let finish (notifyTimeout bounds it).
	if a.bg != nil {
		a.bg.close()
	}
	if a.sshAgents != nil {
		targets.ResetManagedSSH(a.sshAgents)
		a.sshAgents.Close()
	}
	if a.machines != nil {
		a.machines.Close()
	}
	if a.plugins != nil {
		_ = a.plugins.Close(context.Background())
	}
	return a.store.Close()
}

// DropSSHKey stops serving a managed SSH key (LOOM-138): the key was
// deleted.
func (a *App) DropSSHKey(id string) {
	a.sshAgents.Drop(id)
}

// DefaultAgentTypes is the production registered-agent-type set:
// "claude-code" and "codex" (LOOM-22 — proves the agent-type interface
// generalizes beyond a single CLI), both built by package agents. Both
// are TierMarker, and Loomux owns the whole completion path (LOOM-75):
// router.launchAgent tells the launched process its marker path
// (LOOMUX_MARKER_PATH, LOOM-32), and each adapter's CompletionHookArgs
// install the hook that touches it — claude's Stop hook via --settings,
// codex's notify via -c — per launch, with nothing configured on the
// target beforehand. The "" entry configures completion detection for
// shell-kind (provisioning) tasks only (router/agenttype.go's own doc
// comment) — it is never a real dispatchable agent type and is
// deliberately excluded from what's offered to the router model by
// dispatchableAgentTypeNames below.
func DefaultAgentTypes() router.AgentTypeRegistry {
	return router.AgentTypeRegistry{
		"": router.AgentType{
			AgentConfig: completion.AgentConfig{Tier: completion.TierIdle},
		},
		"claude-code": agents.ClaudeCode(),
		"codex":       agents.Codex(),
	}
}

// dispatchableAgentTypeNames excludes the "" bookkeeping entry (see
// DefaultAgentTypes) — the router model must never be offered it as a
// choice, since it isn't a real, launchable agent type.
// agentDescriptionsFor maps each dispatchable agent type to its
// description, for the routing prompt (LOOM-88).
func agentDescriptionsFor(agentTypes router.AgentTypeRegistry) map[string]string {
	out := make(map[string]string, len(agentTypes))
	for _, name := range dispatchableAgentTypeNames(agentTypes) {
		if d := agentTypes[name].Description; d != "" {
			out[name] = d
		}
	}
	return out
}

// The orphan sweep (LOOM-93) runs hourly and kills a Loomux session no
// live task owns once it is a day old: long enough to attach to a failed
// turn's pane and look.
const (
	orphanSweepInterval = time.Hour
	orphanTTL           = 24 * time.Hour
)

// A completed task's pane is kept this long, so a person can attach and
// see what the agent did (LOOM-91), checked every finishedPaneSweep.
const (
	finishedPaneGrace = 15 * time.Minute
	finishedPaneSweep = time.Minute
)

// At most notifyBurst notifications go out at once, then one more every
// notifyRefill (LOOM-102): a string of failures mustn't flood a phone.
const (
	notifyBurst  = 5
	notifyRefill = time.Minute
)

// reconcileTimeout bounds startup task reconciliation (LOOM-82).
const reconcileTimeout = 2 * time.Minute

// dispatchSweepInterval is how often dispatches left unfinished with no
// job are swept (LOOM-146).
const dispatchSweepInterval = time.Minute

// retentionSweepInterval is how often StartRetention's sweep runs
// (LOOM-193): retention is counted in days, so hourly is plenty.
const retentionSweepInterval = time.Hour

// runDispatchSweep runs dispatches.SweepOrphans every interval until ctx
// is done.
func runDispatchSweep(ctx context.Context, dispatches *dispatch.Service, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := dispatches.SweepOrphans(ctx); err != nil && logger != nil {
				logger.Error("dispatch sweep failed", "error", err)
			}
		}
	}
}

// lateOutputInterval is how often agents waiting for input are checked
// for output written after their turn was relayed (LOOM-121).
const lateOutputInterval = 15 * time.Second

// runLateOutputRelay relays late agent output (router.RelayLateOutput)
// every interval until ctx is done.
func runLateOutputRelay(ctx context.Context, rtr *router.Router, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rtr.RelayLateOutput(ctx)
		}
	}
}

func dispatchableAgentTypeNames(agentTypes router.AgentTypeRegistry) []string {
	names := make([]string, 0, len(agentTypes))
	for name := range agentTypes {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// Build wires the full domain layer (design spec §2-§7) from cfg:
// storage -> executor factory -> completion detection -> orchestrator
// -> credential resolver -> LLM-backed routing model -> router. On any
// failure after the store opens successfully, the store is closed
// before returning the error.
func Build(cfg Config) (*App, error) {
	return build(cfg, DefaultAgentTypes())
}

// build is Build's parameterized core — split out so tests can wire a
// custom AgentTypeRegistry (e.g. a fast TierIdle agent type against a
// real tmux session) without needing a real "claude" CLI installed,
// while Build's public signature stays fixed to the production
// registry.
func build(cfg Config, agentTypes router.AgentTypeRegistry) (*App, error) {
	var opts []sqlite.Option
	if cfg.MasterKey != nil {
		opts = append(opts, sqlite.WithMasterKey(cfg.MasterKey))
	}
	// Before the store opens, so a bad override leaves nothing to clean up.
	if err := agentTypes.ApplyProfileOverrides(cfg.AgentProfiles); err != nil {
		return nil, fmt.Errorf("app: %s: %w", envAgentProfiles, err)
	}
	if cfg.Logger != nil {
		names := dispatchableAgentTypeNames(agentTypes)
		sort.Strings(names)
		for _, name := range names {
			p := agentTypes[name].Profile
			cfg.Logger.Info("agent launch profile", "agent_type", name,
				"permission_args", p.PermissionArgs, "pre_trust", p.TrustArgs != nil || p.TrustCommand != nil, "prompt_as_arg", p.PromptAsArg)
		}
	}

	store, err := sqlite.Open(cfg.DBPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}

	// Metrics use an isolated registry so tests can call Build repeatedly
	// without panicking on duplicate registration, while production still
	// exposes them via App.Metrics().Handler() (LOOM-103).
	met := metrics.NewMetrics(prometheus.NewRegistry())

	// Passed to both the detector and the router, so they agree on where
	// a TierMarker agent-type's marker files live (LOOM-32): router.
	// launchAgent tells the launched process the path, completion.Detector
	// watches it. Empty means each target's per-user default, which both
	// resolve the same way (completion.ResolveMarkerDir).
	if cfg.TmuxSocket != "" {
		if err := targets.SetTmuxSocket(cfg.TmuxSocket); err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
	}
	targets.SetLocalTargets(!cfg.LocalTargetsOff)
	if !cfg.LocalTargetsOff {
		// Sessions may outlive loomuxd, and with them a tmux server
		// started with loomuxd's full environment (LOOM-141). After
		// SetTmuxSocket: it's this instance's socket that's scrubbed.
		scrubCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := targets.ScrubLocalTmuxEnv(scrubCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
	}
	// Pinned host keys (LOOM-114) are written beside the database, on the
	// data volume; the database is their source, so the files are
	// rewritten from it as needed.
	targets.SetKnownHostsDir(filepath.Join(filepath.Dir(cfg.DBPath), "known_hosts.d"))
	// Managed targets (LOOM-138): keys decrypted only into in-process
	// agents, whose sockets (like the ControlMaster ones) live under
	// TMPDIR, never on the data volume.
	sshAgents := targets.NewAgentPool(filepath.Join(os.TempDir(), "loomux", "agents"))
	relay, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("app: find loomuxd's own path for the SSH proxy relay: %w", err)
	}
	if err := targets.SetManagedSSH(&targets.ManagedSSH{
		Keys:    store.GetSSHKey,
		Agents:  sshAgents,
		KeysDir: filepath.Join(os.TempDir(), "loomux", "keys"),
		Proxy:   cfg.SSHProxy,
		Relay:   relay,
	}); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	markerDir := cfg.MarkerDir
	newExecutor := func(t *registry.Target) (targets.TargetExecutor, error) {
		return targets.NewExecutorWithMetrics(t, met)
	}
	detector := completion.NewDetector(store, newExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, newExecutor, detector, orchestrator.WithMetrics(met))
	creds := credentials.NewResolver(store)

	model, err := llmrouter.New(cfg.Router, dispatchableAgentTypeNames(agentTypes), llmrouter.WithMetrics(met),
		llmrouter.WithAgentDescriptions(agentDescriptionsFor(agentTypes)))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("app: router model: %w", err)
	}
	// Tiers stored through Settings (LOOM-185) override the env's, which
	// stays the bootstrap and the fallback.
	routerSettings, err := llmrouter.NewSettings(context.Background(), store, model, cfg.Router)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("app: %w", err)
	}
	if cfg.Logger != nil {
		for _, v := range routerSettings.Tiers() {
			if v.StoredUnreadable {
				cfg.Logger.Warn("stored router settings don't decrypt with this master key; using the environment's",
					"tier", v.Tier, "source", v.Source)
			}
		}
	}

	threshold := cfg.ReapIdleThreshold
	if threshold == 0 {
		threshold = defaultReapIdleThreshold
	}
	interval := cfg.ReapInterval
	if interval == 0 {
		interval = defaultReapInterval
	}
	var routerOpts []router.Option
	if cfg.Logger != nil {
		routerOpts = append(routerOpts, router.WithLogger(cfg.Logger))
	}
	routerOpts = append(routerOpts, router.WithMetrics(met))
	// The notifier (LOOM-102) hears of finished turns from the dispatch
	// service and of late replies (LOOM-121) from the router.
	bg := &background{}
	var notifier *turnNotifier
	if cfg.Notify.NtfyURL != "" {
		logger := cfg.Logger
		if logger == nil {
			logger = slog.New(slog.DiscardHandler)
		}
		notifier = &turnNotifier{
			store: store,
			filter: notify.NewFilter(notify.NewNtfy(cfg.Notify.NtfyURL, cfg.Notify.NtfyTopic, cfg.Notify.NtfyToken), notify.FilterConfig{
				Events: cfg.Notify.Events, Burst: notifyBurst, Refill: notifyRefill,
			}),
			minDuration: cfg.Notify.MinDuration,
			publicURL:   cfg.Notify.PublicURL,
			logger:      logger,
			bg:          bg,
		}
		routerOpts = append(routerOpts, router.WithLateReplyHook(notifier.lateReply))
	}

	reaper := orchestrator.NewReaper(orch, threshold, orchestrator.WithReaperMetrics(met),
		orchestrator.WithStaleProvisioningAfter(staleProvisioningAfter), orchestrator.WithTurnRetention(cfg.TurnRetention),
		orchestrator.WithEventRetention(cfg.EventRetention))
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		reaper.Run(reaperCtx, interval)
	}()
	// The orphan sweep (LOOM-93) shares the reaper's lifetime.
	bg.Go(func() { orchestrator.NewOrphanSweeper(orch, orphanTTL, cfg.Logger).Run(reaperCtx, orphanSweepInterval) })
	bg.Go(func() {
		orchestrator.NewFinishedPaneSweeper(orch, finishedPaneGrace, cfg.Logger).Run(reaperCtx, finishedPaneSweep)
	})

	// Health probes bypass the metrics-wrapped executor so periodic
	// liveness checks don't pollute the target operation latency/error
	// counters (LOOM-105).
	healthChecker := health.NewChecker(store, targets.NewExecutor, cfg.Router, health.DefaultSidecarAddr)

	// The plugin system (LOOM-178): the instance id labels everything a
	// plugin makes as this server's, generated once and kept in the
	// database so a restored backup keeps its machines.
	instanceID, err := instanceIDFor(store)
	if err != nil {
		stopReaper()
		_ = store.Close()
		return nil, err
	}
	dataDir := filepath.Dir(cfg.DBPath)
	pluginDir := cfg.PluginDir
	if pluginDir == "" {
		pluginDir = filepath.Join(dataDir, "plugins")
	}
	catalog := &plugins.Catalog{BundleDir: cfg.PluginBundleDir, PluginDir: pluginDir, SocketDir: cfg.PluginSocketDir, Logger: cfg.Logger}
	pluginManager := plugins.NewManager(store, catalog,
		protocol.HostInfo{Version: version.Version, InstanceID: instanceID, DataDir: filepath.Join(dataDir, "plugin-data")},
		cfg.Logger, met)
	healthChecker.WithPlugins(func(ctx context.Context) health.ComponentResult {
		report := pluginManager.Health(ctx)
		return health.ComponentResult{Status: report.Status, Error: report.Error, Detail: report.Plugins}
	})
	machines := providers.NewManager(store, pluginManager, instanceID, cfg.Logger, met)
	machines.ActiveTask = providers.ActiveTaskChecker(store)
	machines.DropSSHKey = func(keyID string) { sshAgents.Drop(keyID) }
	pluginManager.MachineCounter = machines.MachineCount
	pluginManager.UninstallMachines = machines.UninstallMachines
	pluginManager.OnNotify = machines.HandleNotify
	routerOpts = append(routerOpts, router.WithMachines(
		func(ctx context.Context, targetID string) (string, bool, string, bool) {
			env, err := machines.Get(ctx, targetID)
			if err != nil {
				return "", false, "", false
			}
			return env.PluginName, !env.Persistent, string(env.Status), true
		},
		func(ctx context.Context, targetID string) error {
			err := machines.EnsureRunning(ctx, targetID)
			if errors.Is(err, providers.ErrNotMachine) {
				return nil
			}
			return err
		},
	))
	bg.Go(func() {
		if err := pluginManager.StartAll(reaperCtx); err != nil && cfg.Logger != nil {
			cfg.Logger.Warn("some plugins didn't start", "error", err.Error())
		}
		reconcileEvery := cfg.TargetProbeInterval
		if reconcileEvery == 0 {
			reconcileEvery = defaultTargetProbeInterval
		}
		machines.RunReconcile(reaperCtx, reconcileEvery)
	})

	rtr := router.New(store, orch, newExecutor, creds, agentTypes, model, markerDir, routerOpts...)
	machines.Probe = func(ctx context.Context, targetID string) error {
		_, _, err := rtr.ProbeTarget(ctx, targetID)
		return err
	}
	// The target health probe (LOOM-86) and the late-output relay
	// (LOOM-121) share the reaper's lifetime too, and Close waits for
	// them: either, mid-flight, writes to the store.
	probeInterval := cfg.TargetProbeInterval
	if probeInterval == 0 {
		probeInterval = defaultTargetProbeInterval
	}
	proberDone := make(chan struct{})
	go func() {
		defer close(proberDone)
		var late sync.WaitGroup
		late.Go(func() { runLateOutputRelay(reaperCtx, rtr, lateOutputInterval) })
		runTargetProber(reaperCtx, rtr, probeInterval)
		late.Wait()
	}()
	dispatchOpts := []dispatch.Option{
		dispatch.WithErrorClassifier(router.ClassifyError),
		dispatch.WithMaxDuration(cfg.DispatchMaxDuration),
		dispatch.WithLogger(cfg.Logger),
	}
	// Tasks a resumed dispatch waits on (LOOM-82), which startup
	// reconciliation leaves to it.
	resumed := make(map[string]bool)
	dispatchOpts = append(dispatchOpts, dispatch.WithResumer(func(ctx context.Context, d *registry.Dispatch) dispatch.RunFunc {
		taskID, run := rtr.ResumeDispatch(ctx, d)
		if run == nil {
			return nil
		}
		resumed[taskID] = true
		return run
	}))
	if notifier != nil {
		dispatchOpts = append(dispatchOpts, dispatch.WithOnFinished(notifier.finished))
	}
	dispatches := dispatch.New(store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		opts := []router.DispatchOption{router.WithDispatchID(d.ID), router.WithUserMessageLogged()}
		if d.WorkspaceHint != "" {
			opts = append(opts, router.WithWorkspaceHint(d.WorkspaceHint))
		}
		if d.ConfirmationID != "" {
			opts = append(opts, router.WithConfirmationID(d.ConfirmationID))
		}
		return rtr.Dispatch(ctx, d.ConversationID, d.Message, opts...)
	}, dispatchOpts...)
	// A restart forgot every offer awaiting a yes (fails closed): their
	// cards show them expired (LOOM-123).
	if n, err := store.ExpirePendingConfirmations(context.Background()); err != nil {
		stopReaper()
		<-reaperDone
		<-proberDone
		bg.close()
		_ = store.Close()
		return nil, fmt.Errorf("app: %w", err)
	} else if n > 0 && cfg.Logger != nil {
		cfg.Logger.Info("offers left by a previous run expired", "count", n)
	}
	// Before anything can submit, startup reconciliation (LOOM-82): every
	// job a previous process left behind is run, resumed or marked
	// interrupted, then the tasks no resumed job is waiting on are
	// settled.
	if _, err := dispatches.Recover(context.Background()); err != nil {
		stopReaper()
		<-reaperDone
		<-proberDone
		bg.close()
		_ = store.Close()
		return nil, fmt.Errorf("app: %w", err)
	}
	// After Recover, dispatches whose job is gone but whose row was never
	// finished (a status write that failed even after retries) are marked
	// interrupted, so their conversation isn't busy until a restart
	// (LOOM-146).
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		runDispatchSweep(reaperCtx, dispatches, dispatchSweepInterval, cfg.Logger)
	}()
	// In the background, bounded: an unreachable target mustn't hold up
	// startup. Close cancels it with the reaper and waits for it.
	bg.Go(func() {
		ctx, cancel := context.WithTimeout(reaperCtx, reconcileTimeout)
		defer cancel()
		rtr.ReconcileTasks(ctx, resumed)
	})
	dispatchDrain := cfg.DispatchDrain
	if dispatchDrain == 0 {
		dispatchDrain = defaultDispatchDrain
	}

	return &App{
		routerSettings:   routerSettings,
		router:           rtr,
		agentTypes:       dispatchableAgentTypeNames(agentTypes),
		cancelTaskDirect: rtr.CancelTask,
		orch:             orch,
		dispatches:       dispatches,
		dispatchDrain:    dispatchDrain,
		store:            store,
		stopReaper:       stopReaper,
		reaperDone:       reaperDone,
		proberDone:       proberDone,
		bg:               bg,
		bgCtx:            reaperCtx,
		sweepDone:        sweepDone,
		retention: retention.Config{
			Sessions:      cfg.SessionRetention,
			Dispatches:    cfg.DispatchRetention,
			Confirmations: cfg.ConfirmationRetention,
		},
		logger:        cfg.Logger,
		metrics:       met,
		healthChecker: healthChecker,
		plugins:       pluginManager,
		machines:      machines,
		sshAgents:     sshAgents,
	}, nil
}

// instanceIDFor reads the server's instance id from the settings table,
// generating and storing one on first start (LOOM-178).
func instanceIDFor(store registry.Store) (string, error) {
	ctx := context.Background()
	id, err := store.GetSetting(ctx, registry.SettingInstanceID)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, registry.ErrNotFound) {
		return "", fmt.Errorf("app: instance id: %w", err)
	}
	id = uuid.NewString()
	if err := store.SetSetting(ctx, registry.SettingInstanceID, id); err != nil {
		return "", fmt.Errorf("app: instance id: %w", err)
	}
	return id, nil
}
