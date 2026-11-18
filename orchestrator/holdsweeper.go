package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// HoldSweepInterval is how often the hold sweeper looks for holds to end
// (docs/design/tui-client-design.md, "Expiry, crashes and restarts"):
// well inside the shortest lease, so a hold ends within seconds of its
// lease running out.
const HoldSweepInterval = 5 * time.Second

// ErrDetachFailed is returned when the tmux clients attached to a held
// task's session couldn't be detached (LOOM-265). The hold it was for is
// left as it was: a hold only ends once nobody can still be typing into
// the pane, so a task never goes back to automation with a person
// possibly still attached.
var ErrDetachFailed = errors.New("orchestrator: could not detach the clients attached to the task")

// HoldSweeper ends the holds nobody keeps (LOOM-265): one whose lease ran
// out without a heartbeat (expired), whose session was logged out,
// revoked or has expired (session_revoked), or whose task ended
// (task_ended). Ending a drive hold for the first two detaches the tmux
// clients attached to the task's session and hands the task back to
// automation, in that order: if they can't be detached, the hold stays
// and the next sweep tries again (it fails closed).
//
// On start, Run first gives every active hold a full lease from then
// (Grace), so a restart or deploy longer than a lease doesn't end the
// holds of clients that are still there and resume heartbeats once the
// API answers.
type HoldSweeper struct {
	orch *Orchestrator
	// sessionTTL and sessionMaxAge are the client API's session lifetime
	// (sliding and absolute, 0 for none): a hold's session past either
	// has expired, as the API would refuse its token.
	sessionTTL, sessionMaxAge time.Duration
	logger                    *slog.Logger

	// warned are the holds whose end has failed and been warned about:
	// a target down for an hour is said once, not every sweep.
	mu     sync.Mutex
	warned map[string]bool
}

// HoldSweeperOption configures a HoldSweeper.
type HoldSweeperOption func(*HoldSweeper)

// WithHoldSessionLifetime makes a hold whose session has expired end as
// session_revoked: ttl is the API's sliding session lifetime and maxAge
// its absolute one (0 for none). Without it only a session that no
// longer exists counts as revoked.
func WithHoldSessionLifetime(ttl, maxAge time.Duration) HoldSweeperOption {
	return func(s *HoldSweeper) { s.sessionTTL, s.sessionMaxAge = ttl, maxAge }
}

// WithHoldSweeperLogger sets where the sweeper logs; the orchestrator's
// logger by default. nil is ignored.
func WithHoldSweeperLogger(l *slog.Logger) HoldSweeperOption {
	return func(s *HoldSweeper) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewHoldSweeper constructs a HoldSweeper over orch's holds, on orch's
// clock.
func NewHoldSweeper(orch *Orchestrator, opts ...HoldSweeperOption) *HoldSweeper {
	s := &HoldSweeper{orch: orch, logger: orch.logger, warned: map[string]bool{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run applies the restart grace, then sweeps every interval until ctx is
// done.
func (s *HoldSweeper) Run(ctx context.Context, interval time.Duration) {
	if n, err := s.Grace(ctx); err != nil {
		s.logger.Error("hold restart grace failed", "error", err)
	} else if n > 0 {
		s.logger.Info("holds given a full lease from the start", "count", n)
	}
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

// Grace gives every active hold a full lease counted from now (its
// ExpiresAt is moved to at least now + its TTL) and returns how many it
// moved.
func (s *HoldSweeper) Grace(ctx context.Context) (int, error) {
	n, err := s.orch.store.ExtendActiveTaskHolds(ctx, s.orch.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("orchestrator: hold grace: %w", err)
	}
	return n, nil
}

// Sweep runs one pass over the active holds and returns how many it
// ended. Each hold is read again just before it is judged, so a
// heartbeat that landed since the list was read keeps its hold.
func (s *HoldSweeper) Sweep(ctx context.Context) int {
	holds, err := s.orch.store.ListActiveTaskHolds(ctx)
	if err != nil {
		s.logger.Error("hold sweep failed", "error", err)
		return 0
	}
	listed := make(map[string]bool, len(holds))
	var sessions map[string]*registry.Session
	if len(holds) > 0 {
		if sessions, err = s.sessions(ctx); err != nil {
			// Judged on their leases alone this time.
			s.logger.Error("hold sweep: listing sessions failed", "error", err)
		}
	}
	ended := 0
	for _, listedHold := range holds {
		listed[listedHold.ID] = true
		h, err := s.orch.store.GetTaskHold(ctx, listedHold.ID)
		if err != nil || !h.Active() {
			continue // ended, or gone with its task, since the list
		}
		reason, err := s.endReason(ctx, h, sessions)
		if err != nil {
			s.warn(h, "", err)
			continue
		}
		if reason == "" {
			continue
		}
		ok, err := s.orch.endHold(ctx, h, reason)
		if err != nil {
			s.warn(h, reason, err)
			continue
		}
		s.forget(h.ID)
		if ok {
			ended++
		}
	}
	s.mu.Lock()
	for id := range s.warned {
		if !listed[id] {
			delete(s.warned, id)
		}
	}
	s.mu.Unlock()
	return ended
}

// sessions returns every session by id.
func (s *HoldSweeper) sessions(ctx context.Context) (map[string]*registry.Session, error) {
	list, err := s.orch.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*registry.Session, len(list))
	for _, sess := range list {
		out[sess.ID] = sess
	}
	return out, nil
}

// endReason says why h should end now, or "" if it shouldn't. sessions
// nil means they couldn't be read: no hold is judged on its session then.
func (s *HoldSweeper) endReason(ctx context.Context, h *registry.TaskHold, sessions map[string]*registry.Session) (registry.HoldEndReason, error) {
	task, err := s.orch.store.GetTask(ctx, h.TaskID)
	if errors.Is(err, registry.ErrNotFound) {
		return "", nil // its holds went with it
	}
	if err != nil {
		return "", err
	}
	if _, err := heldStatus(task); err != nil {
		return registry.HoldEndTaskEnded, nil
	}
	now := s.orch.now().UTC()
	if sessions != nil {
		if sess, ok := sessions[h.SessionID]; !ok || s.sessionExpired(sess, now) {
			return registry.HoldEndSessionRevoked, nil
		}
	}
	if !now.Before(h.ExpiresAt) {
		return registry.HoldEndExpired, nil
	}
	return "", nil
}

// sessionExpired is the client API's own test (api.Server.sessionExpired)
// on the sweeper's lifetime.
func (s *HoldSweeper) sessionExpired(sess *registry.Session, now time.Time) bool {
	if s.sessionTTL > 0 && now.Sub(sess.LastUsedAt) > s.sessionTTL {
		return true
	}
	return s.sessionMaxAge > 0 && now.Sub(sess.CreatedAt) > s.sessionMaxAge
}

// warn logs a hold the sweeper couldn't end, once until it ends or
// another reason comes up.
func (s *HoldSweeper) warn(h *registry.TaskHold, reason registry.HoldEndReason, err error) {
	s.mu.Lock()
	seen := s.warned[h.ID]
	s.warned[h.ID] = true
	s.mu.Unlock()
	if seen {
		return
	}
	s.logger.Warn("hold sweep: could not end a hold; it stays, and the task with it, until it can",
		"task_id", h.TaskID, "hold_id", h.ID, "session_id", h.SessionID, "reason", string(reason), "error", err)
}

func (s *HoldSweeper) forget(holdID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.warned, holdID)
}

// EndSessionHolds ends every active hold of sessionID as
// session_revoked, as endHold does, and returns how many it ended
// (LOOM-265): for a session logged out or revoked. A hold whose clients
// couldn't be detached is left, its error joined into the one returned;
// the sweeper ends it once it can, since its session is gone.
func (o *Orchestrator) EndSessionHolds(ctx context.Context, sessionID string) (int, error) {
	holds, err := o.store.ListActiveTaskHolds(ctx)
	if err != nil {
		return 0, fmt.Errorf("orchestrator: end session holds: %w", err)
	}
	ended := 0
	var errs []error
	for _, h := range holds {
		if h.SessionID != sessionID {
			continue
		}
		ok, err := o.endHold(ctx, h, registry.HoldEndSessionRevoked)
		if err != nil {
			o.logger.Warn("session revoked: could not end its hold; the hold sweeper will",
				"task_id", h.TaskID, "hold_id", h.ID, "session_id", sessionID, "error", err)
			errs = append(errs, err)
			continue
		}
		if ok {
			ended++
		}
	}
	return ended, errors.Join(errs...)
}

// endHold ends h with reason and hands its task back (ReleaseTaskHold's
// one write: only a human-takeover task left with no drive hold goes to
// awaiting-input). A drive hold's clients are detached first, unless its
// task has ended: if they can't be, h is left as it is and the error is
// returned. ended is false if h had already ended.
func (o *Orchestrator) endHold(ctx context.Context, h *registry.TaskHold, reason registry.HoldEndReason) (ended bool, err error) {
	if h.Mode == registry.HoldModeDrive && reason != registry.HoldEndTaskEnded {
		if err := o.detachHoldClients(ctx, h.TaskID); err != nil {
			return false, err
		}
	}
	res, err := o.store.ReleaseTaskHold(ctx, h.TaskID, h.ID, reason, o.now().UTC())
	if errors.Is(err, registry.ErrNotFound) {
		return false, nil // gone with its task
	}
	if err != nil {
		return false, fmt.Errorf("orchestrator: end hold: %w", err)
	}
	if res.TaskReleased {
		o.recordTaskTransition(string(registry.TaskStatusHumanTakeover), string(res.Task.Status), res.Task.Kind)
	}
	if res.Ended {
		o.logger.Info("hold ended", "task_id", h.TaskID, "hold_id", h.ID, "session_id", h.SessionID,
			"client_label", h.ClientLabel, "reason", string(reason), "task_released", res.TaskReleased)
	}
	return res.Ended, nil
}

// detachHoldClients detaches every tmux client attached to taskID's
// session, through its target's executor (ErrDetachFailed if it can't).
// A task, workspace or target that no longer exists, a task with no
// session, or a local target this server may not run has no pane anyone
// can be attached to through Loomux: nothing to do.
func (o *Orchestrator) detachHoldClients(ctx context.Context, taskID string) error {
	task, err := o.store.GetTask(ctx, taskID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDetachFailed, err)
	}
	if task.TmuxSession == "" {
		return nil
	}
	exec, err := o.executorFor(ctx, task)
	if errors.Is(err, registry.ErrNotFound) || errors.Is(err, targets.ErrLocalTargetsOff) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: task %q: %w", ErrDetachFailed, task.ID, err)
	}
	if err := exec.DetachClients(ctx, task.TmuxSession); err != nil {
		return fmt.Errorf("%w: task %q: %w", ErrDetachFailed, task.ID, err)
	}
	return nil
}
