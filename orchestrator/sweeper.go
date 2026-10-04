package orchestrator

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// listLoomuxSessions lists the sessions on Loomux's own tmux server, one
// "name created-unix-time" per line. No server running is no sessions,
// not an error.
var listLoomuxSessions = "tmux -L " + targets.TmuxSocket + " list-sessions -F '#{session_name} #{session_created}' 2>/dev/null; true"

// OrphanSweeper finds, per target, tmux sessions on Loomux's socket that
// no live task owns — left by failed launches, restarts, a task row
// failed while its pane was kept for inspection — reports them, and
// kills them once they are older than its TTL (LOOM-93). It only ever
// looks at the Loomux socket (targets.TmuxSocket) and at sessions named
// with targets.SessionPrefix: nothing else is touched.
type OrphanSweeper struct {
	orch   *Orchestrator
	ttl    time.Duration
	logger *slog.Logger
}

// NewOrphanSweeper constructs an OrphanSweeper killing orphans older
// than ttl. A nil logger discards.
func NewOrphanSweeper(orch *Orchestrator, ttl time.Duration, logger *slog.Logger) *OrphanSweeper {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &OrphanSweeper{orch: orch, ttl: ttl, logger: logger}
}

// Run sweeps every interval until ctx is done.
func (s *OrphanSweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs one pass over every target and returns how many orphans it
// killed. A target that can't be reached is skipped and logged.
func (s *OrphanSweeper) Sweep(ctx context.Context) int {
	tasks, err := s.orch.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("orphan sweep failed", "error", err)
		return 0
	}
	owned := make(map[string]bool)
	for _, t := range tasks {
		if t.Status != registry.TaskStatusCompleted && t.Status != registry.TaskStatusFailed {
			owned[t.TmuxSession] = true
		}
	}
	targetList, err := s.orch.store.ListTargets(ctx)
	if err != nil {
		s.logger.Error("orphan sweep failed", "error", err)
		return 0
	}
	killed := 0
	now := time.Now()
	for _, target := range targetList {
		log := s.logger.With("target_id", target.ID, "target", target.Name)
		exec, err := s.orch.newExecutor(target)
		if err != nil {
			log.Warn("orphan sweep skipped a target", "error", err)
			continue
		}
		out, err := exec.RunOnce(ctx, listLoomuxSessions)
		if err != nil {
			log.Warn("orphan sweep skipped a target", "error", err)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			name, created, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || !strings.HasPrefix(name, targets.SessionPrefix) || owned[name] {
				continue
			}
			secs, err := strconv.ParseInt(created, 10, 64)
			if err != nil {
				continue
			}
			age := now.Sub(time.Unix(secs, 0))
			if age < s.ttl {
				log.Info("orphaned loomux session", "session", name, "age", age.Round(time.Minute).String())
				continue
			}
			if err := exec.KillSession(ctx, name); err != nil {
				log.Warn("orphaned loomux session not killed", "session", name, "error", err)
				continue
			}
			killed++
			log.Warn("killed an orphaned loomux session", "session", name, "age", age.Round(time.Minute).String())
		}
	}
	return killed
}

// finishedPaneHorizon is how far back FinishedPaneSweeper looks: a task
// completed longer ago than this was finished before panes were kept
// (LOOM-91), or its target has been unreachable all that time; either
// way the orphan sweep is what cleans up after it.
const finishedPaneHorizon = 24 * time.Hour

// FinishedPaneSweeper tears down the sessions of completed tasks once
// they have been finished for its grace period, marking each reaped
// (LOOM-91): Complete leaves the pane so a person can still attach and
// see what the agent did.
type FinishedPaneSweeper struct {
	orch   *Orchestrator
	grace  time.Duration
	logger *slog.Logger
}

// NewFinishedPaneSweeper constructs a FinishedPaneSweeper with the given
// grace period. A nil logger discards.
func NewFinishedPaneSweeper(orch *Orchestrator, grace time.Duration, logger *slog.Logger) *FinishedPaneSweeper {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &FinishedPaneSweeper{orch: orch, grace: grace, logger: logger}
}

// Run sweeps every interval until ctx is done.
func (s *FinishedPaneSweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs one pass and returns how many panes it tore down. A target
// it can't reach is retried on the next sweep.
func (s *FinishedPaneSweeper) Sweep(ctx context.Context) int {
	tasks, err := s.orch.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("finished-pane sweep failed", "error", err)
		return 0
	}
	now := time.Now().UTC()
	reaped := 0
	for _, t := range tasks {
		if t.Status != registry.TaskStatusCompleted || t.ReapedAt != nil || t.CompletedAt == nil {
			continue
		}
		if age := now.Sub(*t.CompletedAt); age < s.grace || age > finishedPaneHorizon {
			continue
		}
		if err := s.orch.Reap(ctx, t.ID); err != nil {
			s.logger.Warn("finished pane not torn down", "task_id", t.ID, "error", err)
			continue
		}
		reaped++
		s.logger.Info("finished pane torn down", "task_id", t.ID, "session", t.TmuxSession)
	}
	return reaped
}
