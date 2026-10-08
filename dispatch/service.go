// Package dispatch runs chat dispatches as jobs that outlive the HTTP
// request which submitted them (LOOM-80, design spec
// docs/design/async-dispatch-design.md). A job is persisted, together
// with its user message, before Submit returns; it then runs on a
// context the Service owns, so a client going away (a closed tab, a
// sleeping phone, a proxy timeout) never reaches the turn. Results land
// on the job row, which clients read back directly, through the
// conversation's history, or as stream events.
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// Store is the slice of registry.Store a Service needs.
type Store interface {
	CreateDispatch(ctx context.Context, d *registry.Dispatch, userMessage *registry.Message) error
	GetDispatch(ctx context.Context, id string) (*registry.Dispatch, error)
	GetDispatchByIdempotencyKey(ctx context.Context, key string) (*registry.Dispatch, error)
	ListDispatchesByConversation(ctx context.Context, conversationID string) ([]*registry.Dispatch, error)
	ListDispatchesByStatus(ctx context.Context, statuses ...registry.DispatchStatus) ([]*registry.Dispatch, error)
	TransitionDispatch(ctx context.Context, d *registry.Dispatch, from registry.DispatchStatus) error
}

// RunFunc carries out one dispatch — in production, the router's
// Dispatch — and returns the chat reply.
type RunFunc func(ctx context.Context, d *registry.Dispatch) (reply string, err error)

// Request is one submitted chat message.
type Request struct {
	// ConversationID may be empty: Submit then starts a new conversation.
	ConversationID string
	Message        string
	WorkspaceHint  string
	// ConfirmationID names the offer the message answers (LOOM-123).
	ConfirmationID string
	// IdempotencyKey, when set, makes a repeated Submit of the same
	// request return the original dispatch instead of starting another.
	IdempotencyKey string
}

// MaxIDLen is the longest client-supplied id Submit accepts: a
// conversation_id or a workspace_hint (LOOM-154).
const MaxIDLen = 64

// ValidID reports whether s can be a client-supplied id: 1 to MaxIDLen
// ASCII letters, digits, '-' or '_'. A server-minted id (a UUID) always
// is. A conversation id is a path segment everywhere else in the API
// (GET /conversations/{id}), so one that isn't could be stored but never
// fetched back.
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > MaxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Validate checks what a client supplies: a message, and, when set, a
// conversation id and workspace hint that are ValidIDs. The error wraps
// ErrInvalidRequest and names the field.
func (req Request) Validate() error {
	if req.Message == "" {
		return fmt.Errorf("%w: message is required", ErrInvalidRequest)
	}
	if req.ConversationID != "" && !ValidID(req.ConversationID) {
		return fmt.Errorf("%w: conversation_id must be 1 to %d letters, digits, '-' or '_'", ErrInvalidRequest, MaxIDLen)
	}
	if req.WorkspaceHint != "" && !ValidID(req.WorkspaceHint) {
		return fmt.Errorf("%w: workspace_hint must be 1 to %d letters, digits, '-' or '_'", ErrInvalidRequest, MaxIDLen)
	}
	return nil
}

var (
	// ErrInvalidRequest: the request can't be dispatched as given. The
	// error Submit returns wraps it and says what was wrong.
	ErrInvalidRequest = errors.New("dispatch: invalid request")
	// ErrKeyReused: the idempotency key was already used for a
	// different request.
	ErrKeyReused = errors.New("dispatch: idempotency key was already used for a different request")
	// ErrShuttingDown: the Service no longer accepts dispatches.
	ErrShuttingDown = errors.New("dispatch: server is shutting down")
	// ErrNotRunning: Cancel was asked for a job that has already ended,
	// or isn't running in this process.
	ErrNotRunning = errors.New("dispatch: not running")
	// errMaxDuration is a job context's cause once it has run for the
	// Service's maximum duration.
	errMaxDuration = errors.New("dispatch: exceeded the maximum dispatch duration")
)

// BusyError is returned when the conversation already has a dispatch in
// flight. LOOM-83 owns serializing a conversation's dispatches; until
// then the second one is refused and told which one is running.
type BusyError struct {
	DispatchID string
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("dispatch: conversation already has dispatch %s in flight", e.DispatchID)
}

const (
	// defaultMaxDuration backstops a whole job. Each agent turn is
	// already bounded (LOOM-76, an hour by default) and every routing or
	// relay call has its own timeout; this covers what lies outside
	// them, e.g. a provisioning wait followed by a full-length turn.
	defaultMaxDuration = 2 * time.Hour
	// cancelGrace is how long Shutdown waits for interrupted jobs to
	// unwind once their contexts are cancelled.
	cancelGrace = 5 * time.Second
)

// Option configures a Service.
type Option func(*Service)

// Defaults for retrying a job's status writes: 100ms doubling, six tries,
// about 3 s of waiting on top of the store's own busy timeout.
const (
	defaultWriteAttempts = 6
	defaultWriteBackoff  = 100 * time.Millisecond
)

// WithWriteRetry overrides how a job's status writes are retried
// (LOOM-146): attempts in all, waiting backoff after the first failure
// and doubling. For tests.
func WithWriteRetry(attempts int, backoff time.Duration) Option {
	return func(s *Service) {
		s.writeAttempts, s.writeBackoff = attempts, backoff
	}
}

// WithMaxDuration overrides the per-job ceiling (defaultMaxDuration).
func WithMaxDuration(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.maxDuration = d
		}
	}
}

// WithLogger sets where job lifecycle logs go. Defaults to discarding.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithErrorClassifier maps a failed run's error to the class recorded on
// the job. Without one every failure is ErrorClassInternal (bar the
// Service's own timeout).
func WithErrorClassifier(f func(error) registry.ErrorClass) Option {
	return func(s *Service) {
		if f != nil {
			s.classify = f
		}
	}
}

// ResumeFunc is offered each job a previous process left running
// (LOOM-82). It returns how to carry that job on — in production, by
// waiting on its agent's still-running turn — or nil when it can't be,
// and the job is marked interrupted.
type ResumeFunc func(ctx context.Context, d *registry.Dispatch) RunFunc

// WithResumer sets how Recover carries on jobs left running (LOOM-82).
// Without one they are all marked interrupted.
func WithResumer(f ResumeFunc) Option {
	return func(s *Service) { s.resume = f }
}

// WithOnFinished calls f with each job that ran to its end, succeeded or
// failed, once its result is recorded (LOOM-102) — not with one
// interrupted by shutdown. f runs on the job's goroutine before waiters
// are released, so it must return quickly; d is a copy f may keep.
func WithOnFinished(f func(d *registry.Dispatch)) Option {
	return func(s *Service) { s.onFinished = f }
}

// Service accepts, runs and tracks dispatch jobs.
type Service struct {
	store       Store
	run         RunFunc
	resume      ResumeFunc
	onFinished  func(d *registry.Dispatch)
	maxDuration time.Duration
	logger      *slog.Logger
	classify    func(error) registry.ErrorClass
	// writeAttempts and writeBackoff bound the retries of a job's own
	// status writes (LOOM-146): a failed one would otherwise leave the
	// row queued or running, and its conversation busy, until a restart.
	writeAttempts int
	writeBackoff  time.Duration

	// mu guards closing and jobs, and is held across a Submit's insert
	// and registration, so Shutdown never misses a job that is being
	// created as it starts.
	mu      sync.Mutex
	closing bool
	jobs    map[string]*job
	wg      sync.WaitGroup
}

// job is a dispatch running in this process.
type job struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// New returns a Service that runs each job with run.
func New(store Store, run RunFunc, opts ...Option) *Service {
	s := &Service{
		store:       store,
		run:         run,
		maxDuration: defaultMaxDuration,
		logger:      slog.New(slog.DiscardHandler),
		classify:    func(error) registry.ErrorClass { return registry.ErrorClassInternal },
		jobs:        make(map[string]*job),

		writeAttempts: defaultWriteAttempts,
		writeBackoff:  defaultWriteBackoff,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Submit persists req as a queued job, with its user message, and starts
// it. It returns once the job is stored — ctx bounds only that, never the
// job. A repeat of an earlier request with the same IdempotencyKey
// returns the earlier job and starts nothing.
func (s *Service) Submit(ctx context.Context, req Request) (*registry.Dispatch, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	// Hashed as the client sent it, before a new conversation gets its
	// id: a retry of a new conversation's first message (same key, no
	// conversation_id) must match the original, not look reused (LOOM-140).
	hash := requestHash(req)
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
	}

	if req.IdempotencyKey != "" {
		if d, err := s.existing(ctx, req.IdempotencyKey, hash); d != nil || err != nil {
			return d, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, ErrShuttingDown
	}

	d := &registry.Dispatch{
		ID:             uuid.NewString(),
		ConversationID: req.ConversationID,
		Message:        req.Message,
		WorkspaceHint:  req.WorkspaceHint,
		ConfirmationID: req.ConfirmationID,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    hash,
		Status:         registry.DispatchStatusQueued,
	}
	userMessage := &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: req.ConversationID,
		Role:           registry.MessageRoleUser,
		Content:        req.Message,
	}
	if err := s.store.CreateDispatch(ctx, d, userMessage); err != nil {
		return s.createConflict(ctx, req, hash, err)
	}

	// The runner owns d from here on; the caller gets a snapshot.
	queued := copyDispatch(d)
	s.start(d, s.run)

	s.logger.Info("dispatch queued", "dispatch_id", d.ID, "conversation_id", d.ConversationID,
		"idempotency_key_set", req.IdempotencyKey != "")
	return queued, nil
}

// existing returns the dispatch already holding key, if it was made for
// the same request; ErrKeyReused if it wasn't; nil, nil if none.
func (s *Service) existing(ctx context.Context, key, hash string) (*registry.Dispatch, error) {
	d, err := s.store.GetDispatchByIdempotencyKey(ctx, key)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dispatch: submit: %w", err)
	}
	if d.RequestHash != hash {
		return nil, ErrKeyReused
	}
	return d, nil
}

// createConflict explains a failed insert. A racing submit with the same
// key may have won either unique index, so the key is checked before
// the conversation is called busy.
func (s *Service) createConflict(ctx context.Context, req Request, hash string, err error) (*registry.Dispatch, error) {
	keyed := req.IdempotencyKey != ""
	if keyed && (errors.Is(err, registry.ErrIdempotencyKeyExists) || errors.Is(err, registry.ErrConversationBusy)) {
		if d, kerr := s.existing(ctx, req.IdempotencyKey, hash); d != nil || kerr != nil {
			return d, kerr
		}
	}
	if errors.Is(err, registry.ErrConversationBusy) {
		all, lerr := s.store.ListDispatchesByConversation(ctx, req.ConversationID)
		if lerr != nil {
			return nil, fmt.Errorf("dispatch: submit: %w", lerr)
		}
		for _, d := range all {
			if !d.Status.Terminal() {
				return nil, &BusyError{DispatchID: d.ID}
			}
		}
	}
	return nil, fmt.Errorf("dispatch: submit: %w", err)
}

// start runs d as a job with run. Callers hold s.mu.
func (s *Service) start(d *registry.Dispatch, run RunFunc) {
	jobCtx, cancel := context.WithCancelCause(context.Background())
	j := &job{cancel: cancel, done: make(chan struct{})}
	s.jobs[d.ID] = j
	s.wg.Add(1)
	go s.runJob(jobCtx, j, d, run)
}

// runJob runs d with run. A queued d is moved to running first; a d
// already running is one Recover is carrying on (LOOM-82).
func (s *Service) runJob(ctx context.Context, j *job, d *registry.Dispatch, run RunFunc) {
	defer s.wg.Done()
	defer func() {
		// Gone from jobs before done closes: a Cancel after Wait returns
		// must not find the job and report it cancelled.
		s.mu.Lock()
		delete(s.jobs, d.ID)
		s.mu.Unlock()
		close(j.done)
	}()
	ctx, cancel := context.WithTimeoutCause(ctx, s.maxDuration, errMaxDuration)
	defer cancel()
	log := s.logger.With("dispatch_id", d.ID, "conversation_id", d.ConversationID)
	// Bookkeeping writes must land even when the job's own context has
	// just been cancelled.
	bookCtx := context.WithoutCancel(ctx)

	started := time.Now().UTC()
	if d.Status == registry.DispatchStatusQueued {
		d.Status = registry.DispatchStatusRunning
		d.StartedAt = &started
		if err := s.transition(bookCtx, log, d, registry.DispatchStatusQueued); err != nil {
			if errors.Is(err, registry.ErrDispatchStateChanged) {
				log.Info("dispatch not started", "reason", err)
			} else {
				log.Error("dispatch not started: its status couldn't be recorded", "error", err)
			}
			return
		}
	}

	reply, err := s.safeRun(ctx, d, run)
	if errors.Is(context.Cause(ctx), orchestrator.ErrInterrupted) {
		// Cut off by shutdown: the row stays running for the next
		// start's Recover to resume or interrupt (LOOM-82).
		log.Info("dispatch left running for the next start")
		return
	}

	finished := time.Now().UTC()
	d.FinishedAt = &finished
	if err == nil {
		d.Status = registry.DispatchStatusSucceeded
		d.Reply = reply
	} else {
		d.Status = registry.DispatchStatusFailed
		d.Error = err.Error()
		d.ErrorClass = s.classify(err)
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errMaxDuration):
			d.ErrorClass = registry.ErrorClassTimeout
		case errors.Is(cause, orchestrator.ErrCancelled):
			d.ErrorClass = registry.ErrorClassCancelled
			d.Error = "cancelled by the user"
		}
	}
	err = s.transition(bookCtx, log, d, registry.DispatchStatusRunning)
	switch {
	case errors.Is(err, registry.ErrDispatchStateChanged):
		// Shutdown marked it interrupted first; that stands.
		log.Info("dispatch finished after being interrupted", "status", string(d.Status))
	case err != nil:
		// Retried and still failing: SweepOrphans marks the row
		// interrupted once this job is gone, so the conversation isn't
		// left busy until a restart.
		log.Error("dispatch result not recorded", "error", err)
	case d.Status == registry.DispatchStatusFailed:
		log.Error("dispatch failed", "error_class", string(d.ErrorClass), "duration_ms", finished.Sub(started).Milliseconds())
	default:
		log.Info("dispatch succeeded", "duration_ms", finished.Sub(started).Milliseconds())
	}
	if err == nil && s.onFinished != nil {
		s.onFinished(copyDispatch(d))
	}
}

// transition writes d's new status, retrying a failure (LOOM-146) other
// than the row having moved on (ErrDispatchStateChanged), which no retry
// can fix.
func (s *Service) transition(ctx context.Context, log *slog.Logger, d *registry.Dispatch, from registry.DispatchStatus) error {
	wait := s.writeBackoff
	for attempt := 1; ; attempt++ {
		err := s.store.TransitionDispatch(ctx, d, from)
		if attempt > 1 && errors.Is(err, registry.ErrDispatchStateChanged) {
			// An earlier attempt may have landed though it reported an
			// error: the row already says what we were writing.
			if cur, gerr := s.store.GetDispatch(ctx, d.ID); gerr == nil && cur.Status == d.Status {
				return nil
			}
		}
		if err == nil || errors.Is(err, registry.ErrDispatchStateChanged) || errors.Is(err, registry.ErrNotFound) ||
			attempt >= s.writeAttempts || s.isClosing() {
			// Shutting down: the row stays as it is for the next start's
			// Recover, rather than holding the drain up with retries.
			return err
		}
		log.Warn("dispatch status write failed, retrying", "status", string(d.Status), "attempt", attempt, "error", err)
		time.Sleep(wait)
		wait *= 2
	}
}

func (s *Service) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// safeRun turns a panic in run into a failed job rather than a dead
// server.
func (s *Service) safeRun(ctx context.Context, d *registry.Dispatch, run RunFunc) (reply string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("dispatch: run panicked: %v", p)
		}
	}()
	return run(ctx, copyDispatch(d))
}

// Wait blocks until dispatch id is finished or ctx is done, and returns
// its row. ctx ending only stops the waiting: the job carries on.
func (s *Service) Wait(ctx context.Context, id string) (*registry.Dispatch, error) {
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j != nil {
		select {
		case <-j.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Get(ctx, id)
}

// Get returns dispatch id as stored.
func (s *Service) Get(ctx context.Context, id string) (*registry.Dispatch, error) {
	return s.store.GetDispatch(ctx, id)
}

// Cancel stops the running job id (LOOM-99): its context is cancelled
// with cause orchestrator.ErrCancelled, which the router answers by
// interrupting the agent and failing its task, and the job then ends
// failed with class cancelled. It returns once the job has been told,
// not once it has ended: the stream reports that. registry.ErrNotFound
// for an unknown id; ErrNotRunning for one that has ended.
func (s *Service) Cancel(ctx context.Context, id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	if ok {
		j.cancel(orchestrator.ErrCancelled)
		return nil
	}
	if _, err := s.store.GetDispatch(ctx, id); err != nil {
		return err
	}
	return ErrNotRunning
}

// ListByConversation returns a conversation's dispatches, oldest first.
func (s *Service) ListByConversation(ctx context.Context, conversationID string) ([]*registry.Dispatch, error) {
	return s.store.ListDispatchesByConversation(ctx, conversationID)
}

// Shutdown stops accepting dispatches and gives running ones until ctx
// is done to finish. Any still running then have their contexts
// cancelled with cause orchestrator.ErrInterrupted, which tells the
// router to leave their tasks running, and their rows are left running
// too: the next process's Recover resumes them (LOOM-82), so a deploy
// mid-turn still delivers the reply.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
	}

	s.mu.Lock()
	running := make(map[string]*job, len(s.jobs))
	for id, j := range s.jobs {
		running[id] = j
	}
	s.mu.Unlock()

	for _, j := range running {
		j.cancel(orchestrator.ErrInterrupted)
	}
	s.logger.Warn("dispatch drain timed out; left in-flight dispatches for the next start to resume", "count", len(running))

	select {
	case <-drained:
		return nil
	case <-time.After(cancelGrace):
		return fmt.Errorf("dispatch: shutdown: dispatches still unwinding after %s", cancelGrace)
	}
}

// Recover deals with every dispatch a previous process left unfinished
// (LOOM-82), and returns how many it marked interrupted. A queued job
// never started, so it is simply run now. A running one is offered to
// the resumer, which can carry it on — its agent may still be working —
// and is otherwise marked interrupted. Call it once at startup, before
// Submit.
func (s *Service) Recover(ctx context.Context) (int, error) {
	left, err := s.store.ListDispatchesByStatus(ctx, registry.DispatchStatusQueued, registry.DispatchStatusRunning)
	if err != nil {
		return 0, fmt.Errorf("dispatch: recover: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range left {
		log := s.logger.With("dispatch_id", d.ID, "conversation_id", d.ConversationID)
		if d.Status == registry.DispatchStatusQueued {
			log.Info("running a dispatch a previous run left queued")
			s.start(d, s.run)
			continue
		}
		if s.resume != nil {
			if run := s.resume(ctx, copyDispatch(d)); run != nil {
				log.Info("resuming a dispatch a previous run left running")
				s.start(d, run)
				continue
			}
		}
		if s.markInterrupted(ctx, d, "interrupted: the server restarted before this dispatch finished") {
			n++
		}
	}
	if n > 0 {
		s.logger.Warn("marked dispatches left by a previous run as interrupted", "count", n)
	}
	return n, nil
}

// SweepOrphans marks interrupted every queued or running dispatch with
// no job in this process (LOOM-146): one whose own status write failed
// even after retries, which would otherwise hold its conversation busy
// (409) until the next restart's Recover. It must only run after
// Recover, and does nothing once Shutdown has begun: the rows a shutdown
// leaves running are for the next start to resume.
func (s *Service) SweepOrphans(ctx context.Context) (int, error) {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return 0, nil
	}
	left, err := s.store.ListDispatchesByStatus(ctx, registry.DispatchStatusQueued, registry.DispatchStatusRunning)
	if err != nil {
		return 0, fmt.Errorf("dispatch: sweep: %w", err)
	}
	// Under mu, a job is in jobs from before its row is written (Submit,
	// Recover) until after its last status write: a row listed above
	// with no job here has nothing left to finish it. The writes happen
	// after mu is released, so a slow database doesn't hold up Submit:
	// no job can start for an existing dispatch after Recover, and one a
	// shutdown interrupts was still in jobs when the orphans were taken.
	// One that finished since the list is refused by the store
	// (ErrDispatchStateChanged).
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return 0, nil
	}
	var orphans []*registry.Dispatch
	for _, d := range left {
		if _, live := s.jobs[d.ID]; !live {
			orphans = append(orphans, d)
		}
	}
	s.mu.Unlock()
	n := 0
	for _, d := range orphans {
		reason := "interrupted: its result couldn't be recorded"
		if d.Status == registry.DispatchStatusQueued {
			reason = "interrupted: it couldn't be started"
		}
		if s.markInterrupted(ctx, d, reason) {
			s.logger.Warn("marked a dispatch with no running job as interrupted", "dispatch_id", d.ID, "conversation_id", d.ConversationID)
			n++
		}
	}
	return n, nil
}

func (s *Service) markInterrupted(ctx context.Context, d *registry.Dispatch, reason string) bool {
	if d.Status.Terminal() {
		return false
	}
	from := d.Status
	finished := time.Now().UTC()
	d.Status = registry.DispatchStatusInterrupted
	d.Error = reason
	d.ErrorClass = registry.ErrorClassInterrupted
	d.FinishedAt = &finished
	if err := s.store.TransitionDispatch(ctx, d, from); err != nil {
		if !errors.Is(err, registry.ErrDispatchStateChanged) {
			s.logger.Error("dispatch not interrupted", "dispatch_id", d.ID, "error", err)
		}
		return false
	}
	return true
}

func requestHash(req Request) string {
	h := sha256.New()
	for _, part := range []string{req.ConversationID, req.Message, req.WorkspaceHint, req.ConfirmationID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyDispatch(d *registry.Dispatch) *registry.Dispatch {
	c := *d
	return &c
}
