package providers

import (
	"context"
	"errors"
	"time"

	"github.com/Loomux/server/registry"
)

// Idle machines (LOOM-177's gap G4). A created machine nobody uses keeps
// its pod or container, with its CPU, memory and place in the cluster's
// quota, until someone stops it. With IdleStop set, each reconcile stops
// a persistent running machine once nothing has happened on it for that
// long: no task there is unfinished, and the newest of its tasks' last
// change, its last start and its last hand-over to a dispatch
// (EnsureRunning) is older than IdleStop. The next dispatch to it starts
// it again, at the same address and with its data, as it starts any
// stopped machine. An ephemeral machine is left running: its stop would
// destroy it.

// Activity reports what is happening on a target, for IdleStop: busy
// while any task there is unfinished (running, waiting for input or for
// a person, taken over), and when its tasks last changed (zero for
// none).
type Activity func(ctx context.Context, targetID string) (busy bool, last time.Time, err error)

// TaskActivity is Activity from the store's tasks: those of the
// target's workspaces.
func TaskActivity(store registry.Store) Activity {
	return func(ctx context.Context, targetID string) (bool, time.Time, error) {
		workspaces, err := store.ListWorkspaces(ctx)
		if err != nil {
			return false, time.Time{}, err
		}
		onTarget := map[string]bool{}
		for _, ws := range workspaces {
			if ws.TargetID == targetID {
				onTarget[ws.ID] = true
			}
		}
		if len(onTarget) == 0 {
			return false, time.Time{}, nil
		}
		tasks, err := store.ListTasks(ctx)
		if err != nil {
			return false, time.Time{}, err
		}
		var last time.Time
		for _, t := range tasks {
			if !onTarget[t.WorkspaceID] {
				continue
			}
			if t.Status != registry.TaskStatusCompleted && t.Status != registry.TaskStatusFailed {
				return true, t.UpdatedAt, nil
			}
			if t.UpdatedAt.After(last) {
				last = t.UpdatedAt
			}
		}
		return false, last, nil
	}
}

// errNotIdle: the machine was used between the idle check and its stop.
var errNotIdle = errors.New("providers: the machine was used meanwhile")

// noteRunning keeps a machine's last use (lastUse) from when it was seen
// to start running, and forgets it once it isn't running.
func (m *Manager) noteRunning(envID string, running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !running {
		delete(m.lastUse, envID)
		return
	}
	if _, ok := m.lastUse[envID]; !ok {
		m.lastUse[envID] = m.Now()
	}
}

// markUsed records that a dispatch was handed the machine now: a
// provisioning run may work there for a while before any task exists.
func (m *Manager) markUsed(envID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUse[envID] = m.Now()
}

// usedAt is the machine's last use as this process saw it; one it
// hasn't seen start (it ran before a restart) counts from now, so a
// restart never stops a machine early.
func (m *Manager) usedAt(envID string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastUse[envID]
	if !ok {
		t = m.Now()
		m.lastUse[envID] = t
	}
	return t
}

// idleSince is when the machine went idle, or false while it is busy
// or the activity can't be read.
func (m *Manager) idleSince(ctx context.Context, env *registry.Environment) (time.Time, bool) {
	busy, last, err := m.Activity(ctx, env.TargetID)
	if err != nil || busy {
		return time.Time{}, false
	}
	since := m.usedAt(env.ID)
	if last.After(since) {
		since = last
	}
	return since, true
}

// StopIdle stops each persistent running machine idle for IdleStop, as
// Stop does (it refuses while a task is active), naming why in its
// status reason. A no-op unless IdleStop and Activity are set. Run at
// the end of every reconcile.
func (m *Manager) StopIdle(ctx context.Context) {
	if m.IdleStop <= 0 || m.Activity == nil {
		return
	}
	envs, err := m.store.ListEnvironments(ctx)
	if err != nil {
		m.logger.Warn("idle machines", "error", err.Error())
		return
	}
	for _, env := range envs {
		if env.Status != registry.EnvironmentRunning {
			m.noteRunning(env.ID, false)
			continue
		}
		if !env.Persistent || env.PluginID == "" || m.following(env.ID) || !m.plugins.Running(env.PluginID) {
			continue
		}
		since, idle := m.idleSince(ctx, env)
		if !idle || m.Now().Sub(since) < m.IdleStop {
			continue
		}
		// Checked again under the machine's lock, just before the stop:
		// a dispatch may have taken it meanwhile.
		still := func(env *registry.Environment) bool {
			since, idle := m.idleSince(ctx, env)
			return idle && m.Now().Sub(since) >= m.IdleStop
		}
		reason := "stopped after " + m.IdleStop.String() + " idle; a request to it starts it again"
		if err := m.stop(ctx, env.TargetID, reason, still); err != nil {
			if !errors.Is(err, errNotIdle) && !errors.Is(err, ErrTaskActive) {
				m.logger.Warn("stop idle machine", "environment", env.ID, "error", err.Error())
			}
			continue
		}
		m.logger.Info("stopped idle machine", "environment", env.ID, "target", env.TargetID, "idle", m.Now().Sub(since).Round(time.Minute).String())
	}
}
