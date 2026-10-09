package providers

import (
	"context"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
)

// RunReconcile reconciles every interval until ctx is done, starting at
// once.
func (m *Manager) RunReconcile(ctx context.Context, interval time.Duration) {
	m.Reconcile(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Reconcile(ctx)
		}
	}
}

// Reconcile compares the rows with what each plugin has (design §1.9):
// a creating machine is resumed, a persistent one that vanished is made
// again, an ephemeral one that vanished is lost and its workspaces
// archived, a stopped one found running is stopped, a destroying one is
// finished; and machines a plugin has that no row knows are orphans,
// destroyed once older than OrphanTTL.
func (m *Manager) Reconcile(ctx context.Context) {
	envs, err := m.store.ListEnvironments(ctx)
	if err != nil {
		m.logger.Warn("reconcile machines", "error", err.Error())
		return
	}
	byPlugin := map[string]map[string]bool{}
	for _, env := range envs {
		if env.PluginID != "" {
			if byPlugin[env.PluginID] == nil {
				byPlugin[env.PluginID] = map[string]bool{}
			}
			byPlugin[env.PluginID][env.ID] = true
		}
		if env.Status == registry.EnvironmentDetached || env.PluginID == "" {
			continue
		}
		if !m.plugins.Running(env.PluginID) {
			continue
		}
		m.reconcileOne(ctx, env)
	}
	m.sweepOrphans(ctx, byPlugin)
	m.updateGauge(ctx)
}

// reconcileOne brings one machine's row and the plugin into step.
func (m *Manager) reconcileOne(ctx context.Context, env *registry.Environment) {
	release := m.lock(env.ID)
	defer release()
	env, target, err := m.load(ctx, env.ID)
	if err != nil {
		return
	}
	var pe protocol.Environment
	err = m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: env.ID}, &pe)
	switch {
	case err == nil:
		m.reconcileKnown(ctx, env, target, pe)
	case isNotFound(err):
		m.reconcileMissing(ctx, env, target)
	default:
		// The plugin can't answer right now: leave the row alone.
	}
}

// reconcileKnown: the plugin has the machine.
func (m *Manager) reconcileKnown(ctx context.Context, env *registry.Environment, target *registry.Target, pe protocol.Environment) {
	switch env.Status {
	case registry.EnvironmentStopped:
		if pe.Status == protocol.EnvRunning || pe.Status == protocol.EnvStarting {
			// The row is the intent.
			_ = m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsStop, protocol.IDParams{ID: env.ID}, nil)
		}
	case registry.EnvironmentDestroying:
		if err := m.plugins.Call(ctx, env.PluginID, protocol.MethodTargetsDestroy, protocol.IDParams{ID: env.ID}, nil); err == nil || isNotFound(err) {
			_ = m.store.DeleteEnvironment(ctx, env.ID)
			_ = m.store.DeleteTarget(ctx, target.ID)
			m.releaseTargetKey(ctx, target.SSHKeyRef)
		}
	case registry.EnvironmentError:
		// Left for inspection; a retry is explicit.
	default:
		// creating, starting, running, recreating: take the plugin's word.
		m.mu.Lock()
		_, followed := m.cancels[env.ID]
		m.mu.Unlock()
		if followed && pe.Status != protocol.EnvRunning {
			return // a job is already following it
		}
		if err := m.apply(ctx, env, target, pe); err != nil {
			m.logger.Warn("reconcile machine", "environment", env.ID, "error", err.Error())
			return
		}
		if pe.Status == protocol.EnvLost {
			m.lost(ctx, env, target, "the plugin reports the machine lost")
		}
	}
}

// reconcileMissing: the plugin doesn't have the machine.
func (m *Manager) reconcileMissing(ctx context.Context, env *registry.Environment, target *registry.Target) {
	switch env.Status {
	case registry.EnvironmentCreating:
		// Resume: Create is idempotent on the id, and the deadline
		// starts again.
		m.logger.Info("resuming machine creation", "environment", env.ID, "target", target.Name)
		m.startJob(env.ID, func(ctx context.Context) { m.runCreate(ctx, env.ID) })
	case registry.EnvironmentRunning, registry.EnvironmentStarting, registry.EnvironmentRecreating:
		if env.Persistent {
			m.logger.Warn("machine gone; making it again", "environment", env.ID, "target", target.Name)
			env.Status, env.StatusReason = registry.EnvironmentCreating, ""
			if err := m.store.UpdateEnvironment(ctx, env); err == nil {
				m.startJob(env.ID, func(ctx context.Context) { m.runCreate(ctx, env.ID) })
			}
			return
		}
		m.lost(ctx, env, target, "the machine is gone and its data with it")
	case registry.EnvironmentDestroying:
		_ = m.store.DeleteEnvironment(ctx, env.ID)
		_ = m.store.DeleteTarget(ctx, target.ID)
		m.releaseTargetKey(ctx, target.SSHKeyRef)
	}
}

// lost marks an ephemeral machine lost and archives its workspaces.
func (m *Manager) lost(ctx context.Context, env *registry.Environment, target *registry.Target, reason string) {
	env.Status, env.StatusReason = registry.EnvironmentLost, reason
	if err := m.store.UpdateEnvironment(ctx, env); err != nil {
		m.logger.Warn("record lost machine", "environment", env.ID, "error", err.Error())
		return
	}
	m.logger.Warn("machine lost", "environment", env.ID, "target", target.Name, "reason", reason)
	workspaces, err := m.store.ListWorkspaces(ctx)
	if err != nil {
		return
	}
	for _, ws := range workspaces {
		if ws.TargetID != target.ID || ws.Status == registry.WorkspaceStatusArchived {
			continue
		}
		ws.Status, ws.StatusReason = registry.WorkspaceStatusArchived, "machine lost: "+reason
		if err := m.store.UpdateWorkspace(ctx, ws); err != nil {
			m.logger.Warn("archive workspace of a lost machine", "workspace", ws.Name, "error", err.Error())
		}
	}
	m.updateGauge(ctx)
}

// sweepOrphans destroys machines a plugin has for this instance that no
// row knows, once they are older than OrphanTTL (from when they were
// first seen, or the plugin's created_at if earlier).
func (m *Manager) sweepOrphans(ctx context.Context, known map[string]map[string]bool) {
	views, err := m.plugins.List(ctx)
	if err != nil {
		return
	}
	now := m.Now()
	for _, v := range views {
		if v.Instance.State != "running" || !v.Enabled {
			continue
		}
		hasCreate := false
		for _, c := range v.Capabilities {
			if c == protocol.CapTargetsCreate {
				hasCreate = true
			}
		}
		if !hasCreate {
			continue
		}
		var list protocol.ListResult
		if err := m.plugins.Call(ctx, v.ID, protocol.MethodTargetsList, nil, &list); err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, pe := range list.Environments {
			if known[v.ID][pe.ID] {
				continue
			}
			key := v.ID + "/" + pe.ID
			seen[key] = true
			m.mu.Lock()
			first, ok := m.orphanSeen[key]
			if !ok {
				first = now
				m.orphanSeen[key] = first
			}
			m.mu.Unlock()
			age := now.Sub(first)
			if !pe.CreatedAt.IsZero() && now.Sub(pe.CreatedAt) > age {
				age = now.Sub(pe.CreatedAt)
			}
			if age < m.OrphanTTL {
				m.logger.Warn("orphan machine", "plugin", v.Label, "environment", pe.ID, "age", age.Round(time.Minute).String())
				continue
			}
			m.logger.Warn("destroying orphan machine", "plugin", v.Label, "environment", pe.ID)
			if err := m.plugins.Call(ctx, v.ID, protocol.MethodTargetsDestroy, protocol.IDParams{ID: pe.ID}, nil); err != nil {
				m.logger.Warn("destroy orphan machine", "plugin", v.Label, "environment", pe.ID, "error", err.Error())
			}
		}
		m.mu.Lock()
		for key := range m.orphanSeen {
			if len(key) > len(v.ID) && key[:len(v.ID)] == v.ID && !seen[key] {
				delete(m.orphanSeen, key)
			}
		}
		m.mu.Unlock()
	}
}
