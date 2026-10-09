// Package retention deletes rows the server no longer needs once they
// are old enough (LOOM-193, docs/design/retention-proposal.md): sessions
// a while after they expired, finished dispatches, and resolved
// confirmations. Messages are kept: they are the conversation history.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/Loomux/server/internal/metrics"
)

// Store is what a sweep deletes from; registry.Store satisfies it.
type Store interface {
	DeleteSessionsExpiredBefore(ctx context.Context, lastUsedBefore, createdBefore time.Time) (int, error)
	DeleteDispatchesFinishedBefore(ctx context.Context, cutoff time.Time) (int, error)
	DeleteConfirmationsResolvedBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// Config says how long each kind of row is kept. Zero keeps it forever.
type Config struct {
	// Sessions is how long a session row is kept after it expired.
	Sessions time.Duration
	// SessionTTL and SessionMaxAge are the session lifetime the API
	// enforces (its sliding window and absolute lifetime, 0 for none):
	// a session expires at whichever comes first.
	SessionTTL, SessionMaxAge time.Duration
	// Dispatches is how long a dispatch is kept after it finished.
	Dispatches time.Duration
	// Confirmations is how long a confirmation is kept after it was
	// answered or expired.
	Confirmations time.Duration
}

// Counts is how many rows one sweep deleted.
type Counts struct {
	Sessions, Dispatches, Confirmations int
}

// Sweeper runs the deletes Config asks for.
type Sweeper struct {
	store   Store
	cfg     Config
	metrics *metrics.Metrics
	logger  *slog.Logger
	now     func() time.Time
}

// Option configures a Sweeper.
type Option func(*Sweeper)

// WithMetrics records each sweep's deletes and failures in m.
func WithMetrics(m *metrics.Metrics) Option { return func(s *Sweeper) { s.metrics = m } }

// WithLogger logs each sweep's counts and failures to l.
func WithLogger(l *slog.Logger) Option { return func(s *Sweeper) { s.logger = l } }

// WithClock overrides the time a sweep measures ages against
// (time.Now), so a test decides exactly what is old enough.
func WithClock(now func() time.Time) Option { return func(s *Sweeper) { s.now = now } }

// New returns a Sweeper deleting from store as cfg says.
func New(store Store, cfg Config, opts ...Option) *Sweeper {
	s := &Sweeper{store: store, cfg: cfg, logger: slog.New(slog.DiscardHandler), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	return s
}

// Run sweeps once now and then every interval until ctx is done.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	s.Sweep(ctx)
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

// Sweep runs one pass and returns what it deleted. A delete that fails
// is logged and counted, and the others still run.
func (s *Sweeper) Sweep(ctx context.Context) Counts {
	now := s.now().UTC()
	var c Counts
	if s.cfg.Sessions > 0 {
		// Expired at now-Sessions or earlier: unused since a sliding
		// window before that, or created an absolute lifetime before it.
		expiredBy := now.Add(-s.cfg.Sessions)
		var createdBefore time.Time
		if s.cfg.SessionMaxAge > 0 {
			createdBefore = expiredBy.Add(-s.cfg.SessionMaxAge)
		}
		c.Sessions = s.run("sessions", func() (int, error) {
			return s.store.DeleteSessionsExpiredBefore(ctx, expiredBy.Add(-s.cfg.SessionTTL), createdBefore)
		})
	}
	if s.cfg.Dispatches > 0 {
		c.Dispatches = s.run("dispatches", func() (int, error) {
			return s.store.DeleteDispatchesFinishedBefore(ctx, now.Add(-s.cfg.Dispatches))
		})
	}
	if s.cfg.Confirmations > 0 {
		c.Confirmations = s.run("confirmations", func() (int, error) {
			return s.store.DeleteConfirmationsResolvedBefore(ctx, now.Add(-s.cfg.Confirmations))
		})
	}
	s.metrics.SetRetentionSweep(s.now())
	level := slog.LevelDebug
	if c.Sessions+c.Dispatches+c.Confirmations > 0 {
		level = slog.LevelInfo
	}
	s.logger.Log(ctx, level, "retention sweep",
		"sessions", c.Sessions, "dispatches", c.Dispatches, "confirmations", c.Confirmations)
	return c
}

func (s *Sweeper) run(table string, del func() (int, error)) int {
	n, err := del()
	s.metrics.RecordRetention(table, n, err)
	if err != nil {
		s.logger.Warn("retention sweep failed", "table", table, "deleted", n, "err", err)
	}
	return n
}
