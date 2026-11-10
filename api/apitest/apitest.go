// Package apitest runs a real api.Server for tests in another module
// (LOOM-270, docs/design/tui-client-design.md "How it pins and tests
// against the server's API"). A client such as loomux/tui requires this
// module at a release tag, imports apitest from its _test.go files only,
// and drives the server over HTTP: the same handlers, auth, JSON and
// event stream a deployed server answers with, so a field or status the
// client gets wrong fails its own tests.
//
//	srv := apitest.New(t)
//	token := srv.Login()
//	task := srv.SeedTask(apitest.Task{ConversationID: "conv-1", Status: registry.TaskStatusAwaitingInput})
//	// ... the client under test talks to srv.URL with token ...
//
// What is real: api.Server, a SQLite store in a temporary directory
// (every migration applied), the dispatch-job service, and the
// orchestrator behind takeover, heartbeat and release. What is not: the
// router and its model (Dispatcher answers each dispatch itself), and the
// targets (Executor is an in-memory tmux; nothing is run anywhere). The
// plugin, credential, SSH-key, router-settings, web-bundle and health
// endpoints aren't configured and answer as an unconfigured server does;
// pass api options through WithAPIOptions to add any.
//
// Everything is torn down by the test's cleanup. The package is for
// tests: it isn't linked into loomuxd.
package apitest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/targets"
)

// DefaultPassword is the password a Server accepts unless WithPassword
// says otherwise.
const DefaultPassword = "apitest-password"

// TmuxSocket is the tmux socket name the server's attach-info names.
const TmuxSocket = "loomux-apitest"

// streamPollInterval is how often the server's conversation stream looks
// for changes: far below production's 500 ms, so a test sees an event
// promptly.
const streamPollInterval = 20 * time.Millisecond

// Server is a running test server. Its fields are for seeding and
// inspecting; a client under test should only use URL and a token.
type Server struct {
	// URL is the server's base URL, without a trailing slash; the API is
	// under URL + "/api/v1".
	URL string
	// Password is what POST /api/v1/login accepts.
	Password string
	// Store is the server's store, for seeding what the Seed helpers
	// don't cover and for reading back what a call changed.
	Store registry.Store
	// Dispatcher answers the dispatches clients submit.
	Dispatcher *Dispatcher
	// Executor is the in-memory target every task's session lives on.
	Executor *Executor
	// API is the api.Server itself (for EndStreams, or to serve it
	// through a handler of the test's own).
	API *api.Server

	tb   testing.TB
	orch *orchestrator.Orchestrator
	seq  atomic.Int64
	// workspace is the default workspace, made on first use.
	workspace *registry.Workspace
}

type config struct {
	password string
	apiOpts  []api.Option
	clock    func() time.Time
}

// Option configures New.
type Option func(*config)

// WithPassword sets the password the server accepts.
func WithPassword(password string) Option {
	return func(c *config) { c.password = password }
}

// WithAPIOptions passes more options to api.NewServer, after the ones New
// sets itself, so they win: a session lifetime, another TaskCanceller, an
// endpoint group New leaves unconfigured.
func WithAPIOptions(opts ...api.Option) Option {
	return func(c *config) { c.apiOpts = append(c.apiOpts, opts...) }
}

// WithClock sets the clock holds are acquired, renewed and ended by, so a
// test decides when a lease runs out. Session lifetimes still run on real
// time.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.clock = now }
}

// New starts a Server and registers its teardown with tb.
func New(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	cfg := config{password: DefaultPassword}
	for _, opt := range opts {
		opt(&cfg)
	}

	store, err := sqlite.Open(filepath.Join(tb.TempDir(), "apitest.db"))
	if err != nil {
		tb.Fatalf("apitest: open store: %v", err)
	}
	// The minimum cost: a test server's logins needn't be slow to guess.
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.password), bcrypt.MinCost)
	if err != nil {
		tb.Fatalf("apitest: hash password: %v", err)
	}

	s := &Server{Password: cfg.password, Store: store, Executor: newExecutor(), tb: tb}
	s.Dispatcher = newDispatcher(store)
	s.orch = orchestrator.New(store, func(*registry.Target) (targets.TargetExecutor, error) { return s.Executor, nil },
		noWait{}, orchestrator.WithClock(cfg.clock), orchestrator.WithLogger(slog.New(slog.DiscardHandler)))

	apiOpts := []api.Option{
		api.WithTmuxSocket(TmuxSocket),
		api.WithStreamPollInterval(streamPollInterval),
		api.WithLoginBackoff(time.Millisecond, 10*time.Millisecond),
		api.WithTaskHolds(s.orch),
		api.WithTaskCanceller(canceller{s}),
		api.WithTaskTurns(store),
		api.WithEvents(store),
		api.WithAgentTypes([]string{DefaultAgentType}),
	}
	s.API = api.NewServer(s.Dispatcher.jobs, store, store, store, store, store, store, hash, append(apiOpts, cfg.apiOpts...)...)
	httpSrv := httptest.NewServer(s.API)
	s.URL = httpSrv.URL

	tb.Cleanup(func() {
		// Streams never end by themselves, and Close waits for them.
		s.API.EndStreams()
		httpSrv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Dispatcher.jobs.Shutdown(ctx)
		if err := store.Close(); err != nil {
			tb.Errorf("apitest: close store: %v", err)
		}
	})
	return s
}

// noWait is the orchestrator's completion detector: nothing in a test
// server waits for an agent's turn.
type noWait struct{}

func (noWait) Wait(ctx context.Context, _ *registry.Task) error {
	<-ctx.Done()
	return ctx.Err()
}

// canceller is the server's api.TaskCanceller, standing in for the
// router's: a running task is cancelled through its conversation's
// dispatch in flight if there is one, any other open task is failed with
// class cancelled.
type canceller struct{ s *Server }

func (c canceller) CancelTask(ctx context.Context, taskID string) (string, error) {
	task, err := c.s.Store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	switch task.Status {
	case registry.TaskStatusCompleted, registry.TaskStatusFailed:
		return "", orchestrator.ErrTaskInactive
	case registry.TaskStatusHumanTakeover:
		return "", orchestrator.ErrHumanTakeover
	}
	if task.Status == registry.TaskStatusRunning && task.ConversationID != "" {
		ds, err := c.s.Dispatcher.jobs.ListByConversation(ctx, task.ConversationID)
		if err != nil {
			return "", err
		}
		for _, d := range ds {
			if d.Status.Terminal() {
				continue
			}
			if err := c.s.Dispatcher.jobs.Cancel(ctx, d.ID); err == nil {
				return d.ID, c.fail(ctx, task)
			} else if !errors.Is(err, dispatch.ErrNotRunning) {
				return "", err
			}
		}
	}
	return "", c.fail(ctx, task)
}

func (c canceller) fail(ctx context.Context, task *registry.Task) error {
	return c.s.orch.Fail(ctx, task.ID, task.Status, registry.TaskFailure{Class: registry.ErrorClassCancelled, Reason: "cancelled by the user"})
}

// nextID is a fresh id with prefix, unique within this server.
func (s *Server) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, s.seq.Add(1))
}
