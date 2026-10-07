// Package api is Loomux's client-facing HTTP surface (design spec §9,
// §10 axis 1): a versioned /api/v1/... surface with real, login-gated
// Bearer-token auth in front of app.App.Dispatch — "there is no 'it's
// just me so no auth' shortcut" (spec §9), since clients are reachable
// outside the server process's own trust boundary. See README.md for
// the auth design and its deployment assumptions.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/internal/health"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/version"
)

// APIVersion is the server↔client API version this package serves
// (design spec §10 axis 1) — the "v1" in /api/v1/..., independent of
// version.Version (axis 4, the server's own release version).
const APIVersion = "v1"

// Dispatcher is what Server needs from the domain layer to run chat
// dispatches: the dispatch-job service (LOOM-80), satisfied by
// *dispatch.Service (app.App.Dispatches() in production). Every POST
// /dispatch becomes a job running on a server-owned context; the
// request only decides whether to wait for it.
type Dispatcher interface {
	Submit(ctx context.Context, req dispatch.Request) (*registry.Dispatch, error)
	// Wait blocks until the job is finished or ctx is done; ctx ending
	// never affects the job.
	Wait(ctx context.Context, id string) (*registry.Dispatch, error)
	Get(ctx context.Context, id string) (*registry.Dispatch, error)
	ListByConversation(ctx context.Context, conversationID string) ([]*registry.Dispatch, error)
	// Cancel stops a running job (LOOM-99): registry.ErrNotFound for an
	// unknown id, dispatch.ErrNotRunning for one that has ended.
	Cancel(ctx context.Context, id string) error
}

// TaskCanceller cancels a running task (LOOM-99): through the dispatch
// driving it if there is one, else directly. It returns the dispatch it
// cancelled, if any; registry.ErrNotFound for an unknown task and
// orchestrator.ErrTaskInactive for one that has ended. Satisfied by
// *app.App. Optional: with none configured (WithTaskCanceller), the
// endpoint answers 501.
type TaskCanceller interface {
	CancelTask(ctx context.Context, taskID string) (dispatchID string, err error)
}

// WorkspaceManager deletes and repairs workspaces (LOOM-70). Satisfied
// by *app.App. Optional: with none configured (WithWorkspaceManager),
// DELETE and PATCH /api/v1/workspaces/{id} answer 501.
type WorkspaceManager interface {
	// DeleteWorkspace deletes a workspace and its tasks and kills their
	// sessions, returning those it couldn't kill. registry.ErrNotFound for
	// an unknown id; a *registry.ConflictError says what blocks it.
	DeleteWorkspace(ctx context.Context, id string) ([]string, error)
	// SetWorkspaceStatus moves a workspace to idle (back in service) or
	// archived (out of it); errors as for DeleteWorkspace.
	SetWorkspaceStatus(ctx context.Context, id string, status registry.WorkspaceStatus) error
}

// SessionStore is the session-related slice of registry.Store this
// package needs — satisfied structurally by any registry.Store
// (including *app.App.Store()). ListSessions backs LOOM-47's
// session-listing/revoke-by-id endpoints; DeleteSession already served
// self-revoke-only (/logout) and is reused unchanged for revoke-by-id.
type SessionStore interface {
	CreateSession(ctx context.Context, s *registry.Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*registry.Session, error)
	ListSessions(ctx context.Context) ([]*registry.Session, error)
	TouchSession(ctx context.Context, id string, lastUsedAt time.Time) error
	DeleteSession(ctx context.Context, id string) error
}

// WorkspaceLister is the workspace-listing slice of registry.Store this
// package needs — satisfied structurally by any registry.Store
// (including *app.App.Store()), mirroring SessionStore's narrow-seam
// pattern.
type WorkspaceLister interface {
	ListWorkspaces(ctx context.Context) ([]*registry.Workspace, error)
}

// TaskLister is the unfiltered task-listing slice of registry.Store this
// package needs to serve conversation listing/history (LOOM-18) —
// satisfied structurally by any registry.Store. Unfiltered because a
// conversation isn't pinned to one workspace: the router can route the
// same conversation_id to a different workspace on a later message, so
// grouping/filtering by conversation has to happen above
// ListTasksByWorkspace's per-workspace scope.
type TaskLister interface {
	ListTasks(ctx context.Context) ([]*registry.Task, error)
}

// MessageLister is the message-transcript slice of registry.Store this
// package needs to extend conversation-detail (LOOM-18) with real chat
// turns instead of only task-lifecycle rows (LOOM-31), and to list
// conversations that never touched a task (LOOM-62) — satisfied
// structurally by any registry.Store, mirroring TaskLister.
type MessageLister interface {
	ListMessagesByConversation(ctx context.Context, conversationID string) ([]*registry.Message, error)
	ListConversationActivity(ctx context.Context) ([]*registry.ConversationActivity, error)
	ListConfirmationsByConversation(ctx context.Context, conversationID string) ([]*registry.Confirmation, error)
}

// AttachInfoStore is the get-chain slice of registry.Store LOOM-20's
// attach-info endpoint needs to resolve a task down to the target a
// human would SSH into: task -> its workspace -> that workspace's
// target. Satisfied structurally by any registry.Store, mirroring this
// package's other narrow seams.
type AttachInfoStore interface {
	GetTask(ctx context.Context, id string) (*registry.Task, error)
	GetWorkspace(ctx context.Context, id string) (*registry.Workspace, error)
	GetTarget(ctx context.Context, id string) (*registry.Target, error)
}

// TargetStore is the target-CRUD slice of registry.Store behind the
// /api/v1/targets endpoints (LOOM-59). Those store methods existed and
// were tested from the start but had no production caller at all, which
// left registering a target possible only by hand-writing a row into
// sqlite on the deployment host — and so left a fresh deployment with
// nowhere to dispatch work. Satisfied structurally by any
// registry.Store, mirroring this package's other narrow seams.
type TargetStore interface {
	CreateTarget(ctx context.Context, t *registry.Target) error
	GetTarget(ctx context.Context, id string) (*registry.Target, error)
	ListTargets(ctx context.Context) ([]*registry.Target, error)
	UpdateTarget(ctx context.Context, t *registry.Target) error
	DeleteTarget(ctx context.Context, id string) error
	// ListTargetAgents backs GET /targets/{id}/agents (LOOM-71): the
	// recorded agent CLI availability on a target.
	ListTargetAgents(ctx context.Context, targetID string) ([]*registry.TargetAgent, error)
	// ListTargetHealth backs each target's health in GET /targets
	// (LOOM-86); a target never probed has no entry.
	ListTargetHealth(ctx context.Context) ([]*registry.TargetHealth, error)
}

// TargetProber probes a target's health and agent CLIs now and records
// the results (LOOM-86) — satisfied by *app.App. Optional: with none
// configured (WithTargetProber), the probe endpoint answers 501.
type TargetProber interface {
	ProbeTarget(ctx context.Context, targetID string) (*registry.TargetHealth, []*registry.TargetAgent, error)
}

// TaskTurnStore is what GET /tasks/{id}/transcript reads (LOOM-91) —
// satisfied by any registry.Store.
type TaskTurnStore interface {
	GetTask(ctx context.Context, id string) (*registry.Task, error)
	ListTaskTurnsPage(ctx context.Context, taskID, beforeID string, limit int) ([]*registry.TaskTurn, bool, error)
}

// HealthChecker is the narrow seam the health endpoints need from the
// domain layer (LOOM-105). It is satisfied by *health.Checker.
type HealthChecker interface {
	Shallow(ctx context.Context) health.Result
	Deep(ctx context.Context) health.Result
}

// defaultSessionTTL is the sliding-expiration window: a session stays
// valid as long as it's used at least once within this window: 30 days,
// reasonable for a personal single-user tool used from mobile.
const defaultSessionTTL = 30 * 24 * time.Hour

// defaultLoginBackoffBase/Max are loginThrottle's production timing: a
// mistyped password barely registers (1s), while a sustained brute-force
// attempt tops out waiting 30s between guesses — see throttle.go for why
// this is backoff, not a hard lockout.
const (
	defaultLoginBackoffBase = time.Second
	defaultLoginBackoffMax  = 30 * time.Second
)

// defaultStreamPollInterval is how often the conversation-stream endpoint
// (LOOM-21) re-checks task state for changes worth pushing to a
// connected client — frequent enough to feel live, infrequent enough not
// to hammer sqlite for what's still a polling loop under the hood.
const defaultStreamPollInterval = 500 * time.Millisecond

// streamHeartbeatInterval is how often a connected stream gets an SSE
// comment line, purely to keep the connection alive through the
// reverse-proxy deployment model this API assumes (README) — proxies
// commonly time out an idle-looking connection well before any real
// status change would otherwise be sent.
const streamHeartbeatInterval = 15 * time.Second

// Server is the client-facing HTTP API. It implements http.Handler —
// callers decide how to actually listen (http.ListenAndServe,
// httptest.Server, etc.), keeping this package transport-agnostic about
// TLS/networking (deployment assumption: plain HTTP behind a reverse
// proxy that terminates TLS — see README.md).
type Server struct {
	dispatcher         Dispatcher
	sessions           SessionStore
	workspaces         WorkspaceLister
	tasks              TaskLister
	messages           MessageLister
	attachInfo         AttachInfoStore
	targets            TargetStore
	targetProber       TargetProber
	taskTurns          TaskTurnStore
	events             EventStore
	scanHostKey        HostKeyScanner
	hostKeys           HostKeyStore
	scans              *scanResults
	workspaceManager   WorkspaceManager
	taskCanceller      TaskCanceller
	credentials        CredentialStore
	agentTypes         []string
	health             HealthChecker
	passwordHash       []byte
	sessionTTL         time.Duration
	loginThrottle      *loginThrottle
	streamPollInterval time.Duration
	staticDir          string
	web                WebBundles
	static             http.HandlerFunc
	mux                *http.ServeMux
}

// Option configures a Server constructed via NewServer.
type Option func(*Server)

// WithSessionTTL overrides the default sliding-expiration window.
func WithSessionTTL(d time.Duration) Option {
	return func(s *Server) { s.sessionTTL = d }
}

// WithLoginBackoff overrides /login's default exponential-backoff timing
// (base delay after the first failure, capped at max). Mainly for tests;
// production defaults are defaultLoginBackoffBase/Max.
func WithLoginBackoff(base, max time.Duration) Option {
	return func(s *Server) { s.loginThrottle = newLoginThrottle(base, max) }
}

// WithStreamPollInterval overrides the conversation-stream endpoint's
// default polling interval (see defaultStreamPollInterval). Mainly for
// tests, so they don't wait a full production-length interval per event.
func WithStreamPollInterval(d time.Duration) Option {
	return func(s *Server) { s.streamPollInterval = d }
}

// WithStaticDir configures Server to serve a built single-page-app from
// dir for any request whose path does not start with /api/ — the web
// client's Vite build output (design spec docs/design/web-client-design.md,
// "Hosting / serving integration"; GitHub issue LOOM-33). A request that
// resolves to a real file under dir is served as-is (correct Content-Type
// via http.FileServer); anything else — an unknown path, or a client-side
// route like /conversations/abc123 that only exists in the SPA's own
// router — falls back to dir/index.html, so a hard refresh on a deep link
// gets the SPA shell instead of a 404. Unset (the default): every such
// request still 404s, unchanged from before this option existed.
func WithStaticDir(dir string) Option {
	return func(s *Server) { s.staticDir = dir }
}

// WithTargetProber enables POST /api/v1/targets/{id}/probe (LOOM-86).
func WithTargetProber(p TargetProber) Option {
	return func(s *Server) { s.targetProber = p }
}

// WithWorkspaceManager enables DELETE and PATCH /api/v1/workspaces/{id}
// (LOOM-70).
func WithWorkspaceManager(m WorkspaceManager) Option {
	return func(s *Server) { s.workspaceManager = m }
}

// WithTaskCanceller enables POST /api/v1/tasks/{id}/cancel (LOOM-99).
func WithTaskCanceller(c TaskCanceller) Option {
	return func(s *Server) { s.taskCanceller = c }
}

// WithAgentTypes names the agent types this server can launch, so a
// target's allowed_agent_types can be checked against them (LOOM-122).
// Without it any non-blank name is accepted.
func WithAgentTypes(names []string) Option {
	return func(s *Server) { s.agentTypes = append([]string(nil), names...) }
}

// WithTaskTurns enables GET /api/v1/tasks/{id}/transcript (LOOM-91).
func WithTaskTurns(t TaskTurnStore) Option {
	return func(s *Server) { s.taskTurns = t }
}

// WithHealthChecker sets the checker used by /api/v1/health and
// /api/v1/health/deep (LOOM-105). A nil value is accepted and ignored.
func WithHealthChecker(h HealthChecker) Option {
	return func(s *Server) { s.health = h }
}

// NewServer constructs a Server. passwordHash is a bcrypt hash (see
// HashPassword) — the single v1 user's credential, verified at login;
// never a plaintext password.
func NewServer(dispatcher Dispatcher, sessions SessionStore, workspaces WorkspaceLister, tasks TaskLister, messages MessageLister, attachInfo AttachInfoStore, targets TargetStore, passwordHash []byte, opts ...Option) *Server {
	s := &Server{
		dispatcher:         dispatcher,
		sessions:           sessions,
		workspaces:         workspaces,
		tasks:              tasks,
		messages:           messages,
		attachInfo:         attachInfo,
		targets:            targets,
		passwordHash:       passwordHash,
		sessionTTL:         defaultSessionTTL,
		loginThrottle:      newLoginThrottle(defaultLoginBackoffBase, defaultLoginBackoffMax),
		streamPollInterval: defaultStreamPollInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case s.web != nil:
		s.static = newStaticHandler(s.web.Root)
	case s.staticDir != "":
		dir := s.staticDir
		s.static = newStaticHandler(func() string { return dir })
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/v1/sessions", s.requireAuth(s.handleListSessions))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.requireAuth(s.handleRevokeSession))
	mux.HandleFunc("POST /api/v1/dispatch", s.requireAuth(s.handleDispatch))
	mux.HandleFunc("GET /api/v1/dispatches/{id}", s.requireAuth(s.handleGetDispatch))
	mux.HandleFunc("POST /api/v1/dispatches/{id}/cancel", s.requireAuth(s.handleCancelDispatch))
	mux.HandleFunc("GET /api/v1/workspaces", s.requireAuth(s.handleListWorkspaces))
	mux.HandleFunc("DELETE /api/v1/workspaces/{id}", s.requireAuth(s.handleDeleteWorkspace))
	mux.HandleFunc("PATCH /api/v1/workspaces/{id}", s.requireAuth(s.handlePatchWorkspace))
	mux.HandleFunc("GET /api/v1/conversations", s.requireAuth(s.handleListConversations))
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.requireAuth(s.handleGetConversation))
	mux.HandleFunc("GET /api/v1/conversations/{id}/stream", s.requireAuth(s.handleStream))
	mux.HandleFunc("GET /api/v1/conversations/{id}/events", s.requireAuth(s.handleConversationEvents))
	mux.HandleFunc("GET /api/v1/tasks/{id}/attach-info", s.requireAuth(s.handleAttachInfo))
	mux.HandleFunc("GET /api/v1/tasks/{id}/transcript", s.requireAuth(s.handleTaskTranscript))
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.requireAuth(s.handleCancelTask))
	mux.HandleFunc("POST /api/v1/targets", s.requireAuth(s.handleCreateTarget))
	mux.HandleFunc("GET /api/v1/targets", s.requireAuth(s.handleListTargets))
	mux.HandleFunc("PUT /api/v1/targets/{id}", s.requireAuth(s.handleUpdateTarget))
	mux.HandleFunc("DELETE /api/v1/targets/{id}", s.requireAuth(s.handleDeleteTarget))
	mux.HandleFunc("GET /api/v1/targets/{id}/agents", s.requireAuth(s.handleListTargetAgents))
	mux.HandleFunc("POST /api/v1/targets/{id}/probe", s.requireAuth(s.handleProbeTarget))
	mux.HandleFunc("POST /api/v1/targets/{id}/test", s.requireAuth(s.handleTestTarget))
	mux.HandleFunc("POST /api/v1/targets/{id}/scan-host-key", s.requireAuth(s.handleScanHostKey))
	mux.HandleFunc("POST /api/v1/targets/{id}/pin", s.requireAuth(s.handlePinHostKey))
	mux.HandleFunc("DELETE /api/v1/targets/{id}/pin", s.requireAuth(s.handleUnpinHostKey))
	mux.HandleFunc("GET /api/v1/credentials", s.requireAuth(s.handleListCredentials))
	mux.HandleFunc("POST /api/v1/credentials", s.requireAuth(s.handleCreateCredential))
	mux.HandleFunc("PUT /api/v1/credentials/{id}/value", s.requireAuth(s.handleSetCredentialValue))
	mux.HandleFunc("DELETE /api/v1/credentials/{id}", s.requireAuth(s.handleDeleteCredential))
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/health/deep", s.requireAuth(s.handleHealthDeep))
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)
	mux.HandleFunc("GET /api/v1/web/version", s.requireAuth(s.handleWebVersion))
	mux.HandleFunc("POST /api/v1/web/update", s.requireAuth(s.handleWebUpdate))
	mux.HandleFunc("POST /api/v1/web/rollback", s.requireAuth(s.handleWebRollback))
	s.mux = mux
	return s
}

// ServeHTTP implements http.Handler. Any /api/... path outside /api/v1/
// (a different or unsupported version, or the bare /api/ root) is
// rejected here, before reaching the mux — design spec §10 axis 1: "a
// mismatch is a clear rejection ... not silent breakage," structured
// rather than a bare 404. This check is a simple path-prefix test done
// ahead of routing, not a registered ServeMux pattern, specifically so
// it can never shadow a real /api/v1/... route hit with the wrong HTTP
// method (ServeMux's own 405 for that case is the correct, distinct
// response — a known path used incorrectly, not an unknown version).
//
// Every other path (anything not equal to /api and not starting with
// /api/) goes to the static handler when WithStaticDir was used
// (LOOM-33) — never to mux, so a static build can never shadow or be
// shadowed by an /api/v1/ route. Without WithStaticDir, those paths 404
// via http.NotFound, matching this package's behavior before this option
// existed. The bare "/api" case (no trailing slash) is checked
// explicitly alongside the "/api/" prefix — strings.HasPrefix alone
// would miss it, letting an API-shaped path fall through to the static
// handler and get served the SPA shell instead of the same
// unsupported-path rejection "/api/v2/..." already gets.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			s.handleUnsupportedAPIPath(w, r)
			return
		}
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.static != nil {
		s.static(w, r)
		return
	}
	http.NotFound(w, r)
}

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if wait := s.loginThrottle.wait(); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	var req loginRequest
	if !readJSON(w, r, &req) {
		return
	}

	if err := checkPassword(s.passwordHash, req.Password); err != nil {
		s.loginThrottle.recordFailure()
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}
	s.loginThrottle.recordSuccess()

	token, tokenHash, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	sess := &registry.Session{ID: uuid.NewString(), TokenHash: tokenHash}
	if err := s.sessions.CreateSession(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if err := s.sessions.DeleteSession(r.Context(), sess.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionSummary struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	// Current marks the session backing this very request's Bearer
	// token, so a "your devices" UI can label one row "this device"
	// without ever seeing a token or token hash — this API never
	// returns either (see Session.TokenHash's own doc comment on why).
	Current bool `json:"current"`
}

type listSessionsResponse struct {
	Sessions []sessionSummary `json:"sessions"`
}

// handleListSessions serves GET /api/v1/sessions (LOOM-47): every
// active session — every device/client currently holding a valid Bearer
// token — most-recently-used first (SessionStore.ListSessions's own
// order), so a client can render a "log out other devices" view. Never
// exposes TokenHash; there is no way to turn a listed session back into
// a usable credential.
//
// "Active" is enforced here explicitly: expiration is a sliding TTL
// (sessionTTL) that requireAuth otherwise only checks opportunistically
// — against whichever single token a request happens to present, with
// stale rows left in the store untouched until that token is next used
// (see requireAuth's own doc comment). Left unfiltered, a session past
// its TTL would still show up here as if it were live, even though the
// same token would already be rejected by requireAuth. This applies the
// identical time.Since(LastUsedAt) > sessionTTL check so the listing's
// definition of "active" actually matches requireAuth's.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.sessions.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list sessions")
		return
	}
	current := sessionFromContext(r.Context())
	now := time.Now()

	out := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		if now.Sub(sess.LastUsedAt) > s.sessionTTL {
			continue
		}
		out = append(out, sessionSummary{
			ID:         sess.ID,
			CreatedAt:  sess.CreatedAt,
			LastUsedAt: sess.LastUsedAt,
			Current:    current != nil && sess.ID == current.ID,
		})
	}
	writeJSON(w, http.StatusOK, listSessionsResponse{Sessions: out})
}

// handleRevokeSession serves DELETE /api/v1/sessions/{id} (LOOM-47):
// revoke any session by id, immediately invalidating whatever Bearer
// token it backs — the same underlying operation /logout already
// performs for the presented token, generalized to any id a client
// learned from GET /api/v1/sessions (e.g. "sign out that other
// device"). Revoking the session backing the current request is
// allowed and behaves exactly like /logout; there's no special case for
// it. 404 if id doesn't name an existing session (SessionStore.
// DeleteSession's own registry.ErrNotFound, e.g. it was already revoked
// or never existed).
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sessions.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type workspaceSummary struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	TargetID       string     `json:"target_id"`
	Status         string     `json:"status"`
	Tags           []string   `json:"tags"`
	Description    string     `json:"description"`
	Capabilities   []string   `json:"capabilities"`
	RollingSummary string     `json:"rolling_summary"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	// StatusReason says why the workspace is in its status, e.g. what
	// made it failed (LOOM-77). Omitted when there's nothing to say.
	StatusReason string `json:"status_reason,omitempty"`
}

type listWorkspacesResponse struct {
	Workspaces []workspaceSummary `json:"workspaces"`
}

// handleListWorkspaces returns a summary of every registered workspace
// (id/name/target/status plus tags/description/capabilities/rolling_summary/
// last_used_at), sorted by name (registry.Store's own
// ListWorkspaces order). Wraps WorkspaceLister.ListWorkspaces.
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.workspaces.ListWorkspaces(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list workspaces")
		return
	}
	out := make([]workspaceSummary, 0, len(workspaces))
	for _, ws := range workspaces {
		out = append(out, workspaceSummary{
			ID:             ws.ID,
			Name:           ws.Name,
			TargetID:       ws.TargetID,
			Status:         string(ws.Status),
			Tags:           orEmpty(ws.Tags),
			Description:    ws.Description,
			Capabilities:   orEmpty(ws.Capabilities),
			RollingSummary: ws.RollingSummary,
			LastUsedAt:     ws.LastUsedAt,
			StatusReason:   ws.StatusReason,
		})
	}
	writeJSON(w, http.StatusOK, listWorkspacesResponse{Workspaces: out})
}

type deleteWorkspaceResponse struct {
	// SessionsNotKilled are tmux sessions of the deleted tasks that
	// couldn't be killed (the target unreachable, say); the orphan sweep
	// removes them later.
	SessionsNotKilled []string `json:"sessions_not_killed"`
}

// handleDeleteWorkspace deletes a workspace, its tasks and their tmux
// sessions (LOOM-70). The chat transcript stays; the workspace's files
// on the target are not touched. 409 says what blocks the delete.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.workspaceManager == nil {
		writeError(w, http.StatusNotImplemented, "workspace management is not configured on this server")
		return
	}
	left, err := s.workspaceManager.DeleteWorkspace(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkspaceManageError(w, err, "could not delete workspace")
		return
	}
	writeJSON(w, http.StatusOK, deleteWorkspaceResponse{SessionsNotKilled: orEmpty(left)})
}

type patchWorkspaceRequest struct {
	Status string `json:"status"`
}

// handlePatchWorkspace sets a workspace's status (LOOM-70): "idle" puts a
// failed or archived one back in service once its directory is confirmed
// on the target, "archived" takes one out. Nothing else is settable.
func (s *Server) handlePatchWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.workspaceManager == nil {
		writeError(w, http.StatusNotImplemented, "workspace management is not configured on this server")
		return
	}
	var req patchWorkspaceRequest
	if !readJSON(w, r, &req) {
		return
	}
	status := registry.WorkspaceStatus(req.Status)
	if status != registry.WorkspaceStatusIdle && status != registry.WorkspaceStatusArchived {
		writeError(w, http.StatusBadRequest, `status must be "idle" or "archived"`)
		return
	}
	if err := s.workspaceManager.SetWorkspaceStatus(r.Context(), r.PathValue("id"), status); err != nil {
		writeWorkspaceManageError(w, err, "could not update workspace")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeWorkspaceManageError(w http.ResponseWriter, err error, fallback string) {
	var conflict *registry.ConflictError
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such workspace")
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, conflict.Reason)
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

// orEmpty returns s when non-nil, otherwise an empty (non-nil) slice. It
// keeps JSON serialization of []string fields as [] instead of null.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type conversationSummary struct {
	ConversationID string    `json:"conversation_id"`
	WorkspaceID    string    `json:"workspace_id"`
	Status         string    `json:"status"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Preview (LOOM-45) is the conversation's first-ever message,
	// truncated to previewMaxRunes — a short snippet for a conversation
	// list UI row, matching how most chat clients label a thread by its
	// opening line rather than its most recent one (which is already
	// visible as Status/UpdatedAt). Empty only for a conversation with
	// tasks but no logged message yet (a turn is logged — see logTurn —
	// once its reply exists, so a first turn still in flight has none).
	Preview string `json:"preview"`
}

type listConversationsResponse struct {
	Conversations []conversationSummary `json:"conversations"`
	// HasMore says conversations beyond this response remain: only ever
	// true when ?limit= cut the list short. Lists that can grow carry it
	// so paging can arrive without breaking a client (api/README.md,
	// "Lists and paging").
	HasMore bool `json:"has_more"`
}

// maxListPage bounds ?limit= on the conversation list and on a
// conversation's messages. Neither has a default limit: without one,
// the whole list comes back, as before has_more existed.
const maxListPage = 500

// parseListLimit reads an optional ?limit= value: 0 when absent (no
// limit), an error message when it isn't a number from 1 to maxListPage.
func parseListLimit(raw string) (int, string) {
	if raw == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxListPage {
		return 0, fmt.Sprintf("limit must be a number from 1 to %d", maxListPage)
	}
	return n, ""
}

// previewMaxRunes bounds conversationSummary.Preview's length. 200 is
// generous enough to show a real opening sentence or two in a list row,
// short enough that one long first message can't bloat the whole
// listing response.
const previewMaxRunes = 200

// firstMessagePreview truncates a conversation's first message to
// previewMaxRunes runes (not bytes, so multi-byte UTF-8 text isn't cut
// mid-rune), appending "…" when truncated.
func firstMessagePreview(content string) string {
	runes := []rune(content)
	if len(runes) <= previewMaxRunes {
		return content
	}
	return string(runes[:previewMaxRunes]) + "…"
}

// handleListConversations returns one summary row per distinct
// conversation_id, sorted most-recently-updated first — the "which
// conversation is active" overview. A conversation isn't a stored entity
// (design spec has no such table), so it is assembled from both places
// one leaves a trace:
//
//   - tasks: the conversation's most-recently-updated task supplies
//     WorkspaceID and Status, since the same conversation can span more
//     than one task (LOOM-13 continuation, or a later message routed to a
//     different workspace by the router).
//   - the message log: every logged turn, including answer_directly turns
//     that create no task at all. A conversation made only of those
//     (LOOM-62) has no task to describe it, so it is listed with no
//     WorkspaceID and Status "completed" — every turn already has its
//     reply; nothing is running or waiting on the user.
//
// UpdatedAt is the later of the latest task update and the latest
// message, so a direct-answer follow-up in a task conversation still
// counts as activity. Preview (LOOM-45) is looked up per conversation via
// MessageLister — one query per distinct conversation, which is fine at
// this tool's expected scale (design spec: personal, single-user) and
// mirrors handleGetConversation's own reliance on the same interface.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	limit, msg := parseListLimit(r.URL.Query().Get("limit"))
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list conversations")
		return
	}
	activity, err := s.messages.ListConversationActivity(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list conversations")
		return
	}

	summaries := make(map[string]*conversationSummary, len(activity))
	for _, t := range tasks {
		cur, ok := summaries[t.ConversationID]
		if ok && !t.UpdatedAt.After(cur.UpdatedAt) {
			continue
		}
		summaries[t.ConversationID] = &conversationSummary{
			ConversationID: t.ConversationID,
			WorkspaceID:    t.WorkspaceID,
			Status:         apiTaskStatus(t.Status),
			UpdatedAt:      t.UpdatedAt,
		}
	}
	for _, a := range activity {
		cur, ok := summaries[a.ConversationID]
		if !ok {
			summaries[a.ConversationID] = &conversationSummary{
				ConversationID: a.ConversationID,
				Status:         string(registry.TaskStatusCompleted),
				UpdatedAt:      a.LastMessageAt,
			}
			continue
		}
		if a.LastMessageAt.After(cur.UpdatedAt) {
			cur.UpdatedAt = a.LastMessageAt
		}
	}

	out := make([]conversationSummary, 0, len(summaries))
	for _, c := range summaries {
		out = append(out, *c)
	}
	// Ties broken by id, so a limit's cut is the same on every request.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ConversationID < out[j].ConversationID
	})
	more := false
	if limit > 0 && len(out) > limit {
		out, more = out[:limit], true
	}
	for i := range out {
		messages, err := s.messages.ListMessagesByConversation(r.Context(), out[i].ConversationID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not list conversations")
			return
		}
		if len(messages) > 0 {
			out[i].Preview = firstMessagePreview(messages[0].Content)
		}
	}

	writeJSON(w, http.StatusOK, listConversationsResponse{Conversations: out, HasMore: more})
}

type conversationTask struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Kind        string     `json:"kind"`
	AgentType   string     `json:"agent_type"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Command and ExitCode are a command task's (LOOM-71/72); the
	// failure fields are a failed task's (LOOM-77). All omitted when
	// empty.
	Command       string `json:"command,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	ErrorClass    string `json:"error_class,omitempty"`
	OutputTail    string `json:"output_tail,omitempty"`
	// Attention is the prompt a needs-attention task is stopped at
	// (LOOM-97), for a client to show with its options.
	Attention *registry.Attention `json:"attention,omitempty"`
}

type messageSummary struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	TaskID  string `json:"task_id,omitempty"`
	// DispatchID is the dispatch job the message belongs to (LOOM-80).
	DispatchID string    `json:"dispatch_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type getConversationResponse struct {
	ConversationID string             `json:"conversation_id"`
	Tasks          []conversationTask `json:"tasks"`
	Messages       []messageSummary   `json:"messages"`
	// Dispatches is every dispatch job of the conversation, oldest first
	// (LOOM-80): a client reopening a conversation sees a turn still in
	// flight, or one that failed, here.
	Dispatches []dispatchResponse `json:"dispatches"`
	// Confirmations is every offer the router made in the conversation
	// and how it was answered, oldest first (LOOM-123): a card with
	// Approve and Deny under the offer's reply, by dispatch_id.
	Confirmations []confirmationResponse `json:"confirmations"`
	// HasMore says earlier messages remain; NextBefore is the before=
	// value that fetches them. Messages page like a task transcript
	// (?limit=, ?before=<message id>), but with no default limit: without
	// ?limit= every message comes back and HasMore is false.
	HasMore    bool   `json:"has_more"`
	NextBefore string `json:"next_before,omitempty"`
}

type confirmationResponse struct {
	ID         string `json:"id"`
	DispatchID string `json:"dispatch_id,omitempty"`
	Kind       string `json:"kind"`
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	AgentType  string `json:"agent_type,omitempty"`
	Command    string `json:"command,omitempty"`
	// WorkspaceName is the workspace's name, as target_name is the
	// target's (renamed from "workspace" before 1.0, API v1 freeze
	// review item 10).
	WorkspaceName string     `json:"workspace_name,omitempty"`
	GitRemote     string     `json:"git_remote,omitempty"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// handleGetConversation returns a conversation's full task history (every
// Task row sharing this conversation_id, oldest first) plus its message
// transcript (LOOM-31: every Message row sharing this conversation_id,
// oldest first) — TaskLister.ListTasks's and MessageLister.
// ListMessagesByConversation's own orders, respectively. A conversation_id
// matching zero tasks AND zero messages is a 404, not an empty response —
// unlike handleListConversations, this is a "fetch one thing" endpoint. A
// conversation that only ever produced answer_directly replies has
// messages but no tasks, and must still resolve to 200.
//
// Messages page (api/README.md, "Lists and paging"): ?limit= returns the
// latest limit messages, still oldest first, and ?before=<message id>
// only those before it; has_more and next_before lead to the rest. Tasks,
// dispatches and confirmations are always complete.
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit, msg := parseListLimit(r.URL.Query().Get("limit"))
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	before := r.URL.Query().Get("before")

	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}

	out := make([]conversationTask, 0)
	for _, t := range tasks {
		if t.ConversationID != id {
			continue
		}
		out = append(out, conversationTask{
			ID:            t.ID,
			WorkspaceID:   t.WorkspaceID,
			Kind:          string(t.Kind),
			AgentType:     t.AgentType,
			Status:        apiTaskStatus(t.Status),
			CreatedAt:     t.CreatedAt,
			UpdatedAt:     t.UpdatedAt,
			StartedAt:     t.StartedAt,
			CompletedAt:   t.CompletedAt,
			Command:       t.Command,
			ExitCode:      t.ExitCode,
			FailureReason: t.FailureReason,
			ErrorClass:    string(t.ErrorClass),
			OutputTail:    t.OutputTail,
			Attention:     t.Attention,
		})
	}

	messages, err := s.messages.ListMessagesByConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}
	msgOut := make([]messageSummary, 0, len(messages))
	for _, m := range messages {
		msgOut = append(msgOut, messageSummary{
			ID:         m.ID,
			Role:       string(m.Role),
			Content:    m.Content,
			TaskID:     m.TaskID,
			DispatchID: m.DispatchID,
			CreatedAt:  m.CreatedAt,
		})
	}

	dispatches, err := s.dispatcher.ListByConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}
	dispatchOut := make([]dispatchResponse, 0, len(dispatches))
	for _, d := range dispatches {
		dispatchOut = append(dispatchOut, newDispatchResponse(d))
	}

	confirmations, err := s.messages.ListConfirmationsByConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}
	now := time.Now()
	confOut := make([]confirmationResponse, 0, len(confirmations))
	for _, c := range confirmations {
		status := c.Status
		// Past its deadline, an offer no longer runs on a yes, though its
		// row is only settled by the next message.
		if status == registry.ConfirmationPending && now.After(c.ExpiresAt) {
			status = registry.ConfirmationExpired
		}
		confOut = append(confOut, confirmationResponse{
			ID: c.ID, DispatchID: c.DispatchID, Kind: c.Kind, TargetID: c.TargetID, TargetName: c.TargetName,
			AgentType: c.AgentType, Command: c.Command, WorkspaceName: c.Workspace, GitRemote: c.GitRemote,
			Status: string(status), CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, ResolvedAt: c.ResolvedAt,
		})
	}

	// A conversation is only truly unknown if it has neither task
	// history nor any logged messages — an answer_directly-only
	// conversation (LOOM-31) has messages but zero tasks, and must not
	// 404.
	if len(out) == 0 && len(msgOut) == 0 && len(dispatchOut) == 0 {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	}

	more, nextBefore := false, ""
	if before != "" {
		idx := slices.IndexFunc(msgOut, func(m messageSummary) bool { return m.ID == before })
		if idx < 0 {
			writeError(w, http.StatusBadRequest, "before names no message of this conversation")
			return
		}
		msgOut = msgOut[:idx]
	}
	if limit > 0 && len(msgOut) > limit {
		msgOut, more = msgOut[len(msgOut)-limit:], true
		nextBefore = msgOut[0].ID
	}

	writeJSON(w, http.StatusOK, getConversationResponse{ConversationID: id, Tasks: out, Messages: msgOut, Dispatches: dispatchOut,
		Confirmations: confOut, HasMore: more, NextBefore: nextBefore})
}

type attachTargetInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	User string `json:"user"`
}

type attachInfoResponse struct {
	TaskID      string `json:"task_id"`
	TmuxSession string `json:"tmux_session"`
	// TmuxSocket and AttachCommand (LOOM-93): Loomux sessions live on
	// their own tmux server, so a bare `tmux attach` won't find them.
	TmuxSocket    string           `json:"tmux_socket"`
	AttachCommand string           `json:"attach_command"`
	Target        attachTargetInfo `json:"target"`
}

// handleAttachInfo resolves a task down to the target+session a human
// would SSH into to attach and watch/take over (design spec §4) —
// task -> its workspace -> that workspace's target. Returns the stored
// data as-is, without probing whether the tmux session is still actually
// alive (matching this API's other endpoints, which are thin
// passthroughs over registry state rather than live target probes): a
// completed/reaped/crashed task's info is still returned, and the client
// discovers a dead session the same way a human always would — the
// attach attempt itself.
func (s *Server) handleAttachInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	task, err := s.attachInfo.GetTask(r.Context(), id)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such task")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch task")
		return
	}

	ws, err := s.attachInfo.GetWorkspace(r.Context(), task.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve task's workspace")
		return
	}

	target, err := s.attachInfo.GetTarget(r.Context(), ws.TargetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve workspace's target")
		return
	}

	writeJSON(w, http.StatusOK, attachInfoResponse{
		TaskID:        task.ID,
		TmuxSession:   task.TmuxSession,
		TmuxSocket:    targets.TmuxSocket,
		AttachCommand: targets.AttachCommand(task.TmuxSession),
		Target: attachTargetInfo{
			ID:   target.ID,
			Name: target.Name,
			Kind: string(target.Kind),
			Host: target.Host,
			User: target.User,
		},
	})
}

type taskUpdateEvent struct {
	TaskID      string    `json:"task_id"`
	WorkspaceID string    `json:"workspace_id"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at"`
	// FailureReason/ErrorClass are set when Status is failed (LOOM-77).
	FailureReason string `json:"failure_reason,omitempty"`
	ErrorClass    string `json:"error_class,omitempty"`
}

// handleStream serves GET /api/v1/conversations/{id}/stream (LOOM-21):
// Server-Sent Events reporting task status transitions for one
// conversation, so a client can watch a dispatch progress, and (LOOM-80)
// dispatch_update events for its dispatch jobs, which carry the reply
// once a job succeeds (see sendDispatchUpdates), and (LOOM-121)
// message_added events for messages logged while it is open (see
// sendMessagesAdded). No 404 for an unknown conversation_id: a client may
// open the stream before ever calling dispatch, to catch the very first
// transition. Implemented as a polling loop against TaskLister (the same
// seam LOOM-18's conversation endpoints use) rather than a push-based
// event bus — this repo's established minimal-machinery style (see e.g.
// completion detection's own idle-heuristic tier) — diffing the
// conversation's latest task by (id, status, updated_at) each tick.
// Stays open until the client disconnects or the request context ends;
// it does not auto-close on a terminal task status, so a client can keep
// watching across multiple turns of a long-lived conversation.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	id := r.PathValue("id")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	poll := time.NewTicker(s.streamPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	var lastSent *taskUpdateEvent
	// dispatchSeen is each job's last reported (status, updated_at), so
	// a job is reported whenever it changes (LOOM-80). Jobs already
	// finished when the stream opens are recorded unreported: only what
	// is still in flight is news to a client connecting now.
	var dispatchSeen map[string]dispatchUpdateEvent
	// messagesSeen is every message ID reported, or there when the
	// stream opened (LOOM-121): nil until the first poll.
	var messagesSeen map[string]bool
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-poll.C:
			dispatchSeen = s.sendDispatchUpdates(r.Context(), w, flusher, id, dispatchSeen)
			messagesSeen = s.sendMessagesAdded(r.Context(), w, flusher, id, messagesSeen)
			ev, err := s.latestConversationTaskEvent(r.Context(), id)
			if err != nil || ev == nil {
				continue
			}
			if lastSent != nil && *lastSent == *ev {
				continue
			}
			lastSent = ev
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: task_update\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// messageAddedEvent is the data of a stream's `event: message_added`
// (LOOM-121): a message was logged to the conversation. A client
// refetches the conversation to show it; it matters most for an agent's
// late reply, which no dispatch the client follows carries.
type messageAddedEvent struct {
	MessageID string    `json:"message_id"`
	TaskID    string    `json:"task_id,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// sendMessagesAdded reports conversationID's messages not in seen, and
// returns seen with them added. The first call (seen nil) records what
// is there unreported: only messages logged while the stream is open
// are news.
func (s *Server) sendMessagesAdded(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	conversationID string, seen map[string]bool) map[string]bool {
	messages, err := s.messages.ListMessagesByConversation(ctx, conversationID)
	if err != nil {
		return seen
	}
	first := seen == nil
	if first {
		seen = make(map[string]bool, len(messages))
	}
	for _, m := range messages {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if first {
			continue
		}
		data, err := json.Marshal(messageAddedEvent{MessageID: m.ID, TaskID: m.TaskID,
			Role: string(m.Role), CreatedAt: m.CreatedAt})
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: message_added\ndata: %s\n\n", data)
		flusher.Flush()
	}
	return seen
}

// latestConversationTaskEvent finds conversationID's most-recently-
// updated task (mirroring handleListConversations' own grouping logic,
// scoped to one conversation) and reports it as a taskUpdateEvent. nil,
// nil means no task exists yet for this conversation.
func (s *Server) latestConversationTaskEvent(ctx context.Context, conversationID string) (*taskUpdateEvent, error) {
	tasks, err := s.tasks.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	var latest *registry.Task
	for _, t := range tasks {
		if t.ConversationID != conversationID {
			continue
		}
		if latest == nil || t.UpdatedAt.After(latest.UpdatedAt) {
			latest = t
		}
	}
	if latest == nil {
		return nil, nil
	}
	return &taskUpdateEvent{
		TaskID:        latest.ID,
		WorkspaceID:   latest.WorkspaceID,
		Status:        apiTaskStatus(latest.Status),
		UpdatedAt:     latest.UpdatedAt,
		FailureReason: latest.FailureReason,
		ErrorClass:    string(latest.ErrorClass),
	}, nil
}

type versionResponse struct {
	ServerVersion string `json:"server_version"`
	APIVersion    string `json:"api_version"`
}

// handleHealth is the cheap, unauthenticated liveness/readiness probe
// (LOOM-105). It reports degraded/unhealthy when core dependencies fail,
// but it does not perform expensive per-target probes.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "health checker not configured")
		return
	}
	result := s.health.Shallow(r.Context())
	status := http.StatusOK
	if result.Status == health.StatusUnhealthy {
		status = http.StatusServiceUnavailable
	} else if result.Status == health.StatusDegraded {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, result)
}

// handleHealthDeep is authenticated because it performs probing work and
// returns per-target detail that could help an attacker map the
// deployment. It extends the shallow check with target reachability and
// sidecar SOCKS5 connectivity.
func (s *Server) handleHealthDeep(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "health checker not configured")
		return
	}
	result := s.health.Deep(r.Context())
	status := http.StatusOK
	if result.Status == health.StatusUnhealthy {
		status = http.StatusServiceUnavailable
	} else if result.Status == health.StatusDegraded {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, result)
}

// handleVersion is unauthenticated — clients need to be able to check
// compatibility (design spec §10 axis 1, axis 4) before they've logged
// in, and version strings aren't sensitive.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, versionResponse{ServerVersion: version.Version, APIVersion: APIVersion})
}

type unsupportedVersionResponse struct {
	Error             string   `json:"error"`
	SupportedVersions []string `json:"supported_versions"`
}

func (s *Server) handleUnsupportedAPIPath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, unsupportedVersionResponse{
		Error:             "unsupported or unknown API path",
		SupportedVersions: []string{APIVersion},
	})
}

// newStaticHandler serves a built SPA from dir: a request that maps to a
// real file under dir is served as-is via http.FileServer (correct
// Content-Type from the file extension); anything else — no such file,
// or the path is a directory (including the root "/") — falls back to
// dir/index.html, so a browser refresh on a client-side route (e.g.
// /conversations/abc123, React Router) gets the SPA shell instead of a
// 404 (design spec web-client-design.md, "Hosting / serving
// integration"). Uses http.Dir.Open (not a raw os.Stat) specifically
// because it already rejects ".." path elements — a traversal attempt
// falls into the same "no such file" branch as any other unknown path
// and gets the SPA shell, never an out-of-dir file.
//
// The fallback serves index.html via http.ServeContent directly rather
// than delegating to fileServer with a rewritten request path: net/http's
// own serveFile has a built-in special case that 301-redirects any
// request whose path ends in "/index.html" to "./" (URL canonicalization,
// so /foo/index.html and /foo/ aren't two live URLs for the same page) —
// rewriting every fallback request's path to literally "/index.html" and
// handing it back to fileServer would hit that special case on every
// single fallback and redirect-loop forever.
func newStaticHandler(dir func() string) http.HandlerFunc {
	serveIndex := func(w http.ResponseWriter, r *http.Request, root http.Dir) {
		f, err := root.Open("/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Revalidated on every load: after a deploy an old index.html
		// would point at asset hashes that no longer exist.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", info.ModTime(), f)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Read per request: a web update (LOOM-118) swaps the directory.
		root := http.Dir(dir())
		f, err := root.Open(r.URL.Path)
		if err != nil {
			serveIndex(w, r, root)
			return
		}
		info, statErr := f.Stat()
		f.Close()
		if statErr != nil || info.IsDir() || r.URL.Path == "/index.html" {
			serveIndex(w, r, root)
			return
		}
		// Vite names every built asset by its content hash, so one never
		// changes under its name.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.FileServer(root).ServeHTTP(w, r)
	}
}

type sessionContextKey struct{}

// requireAuth gates next behind a valid Bearer session token: missing/
// malformed header, unknown token, or one past its sliding-expiration
// window all fail closed with 401. A valid session's LastUsedAt is
// refreshed on every request (the sliding half of "sliding expiration").
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}

		sess, err := s.sessions.GetSessionByTokenHash(r.Context(), hashToken(token))
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not verify session")
			return
		}

		if time.Since(sess.LastUsedAt) > s.sessionTTL {
			_ = s.sessions.DeleteSession(r.Context(), sess.ID) // opportunistic cleanup
			writeError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}

		now := time.Now().UTC()
		if err := s.sessions.TouchSession(r.Context(), sess.ID, now); err != nil {
			writeError(w, http.StatusInternalServerError, "could not refresh session")
			return
		}
		sess.LastUsedAt = now

		ctx := context.WithValue(r.Context(), sessionContextKey{}, sess)
		next(w, r.WithContext(ctx))
	}
}

func sessionFromContext(ctx context.Context) *registry.Session {
	sess, _ := ctx.Value(sessionContextKey{}).(*registry.Session)
	return sess
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

// targetResponse is a registry.Target as clients see it. ssh_key_ref is
// no longer part of the API (API v1 freeze review, item 1): nothing ever
// read it — SSH keys come from the mounted secret — so a target's stored
// value is kept but neither shown nor settable.
type targetResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	User string `json:"user"`
	// WorkspaceRoot bounds where dynamic workspaces are provisioned
	// (LOOM-90); empty means $HOME/loomux-workspaces on the target.
	WorkspaceRoot string `json:"workspace_root"`
	// PermissionMode: empty (each agent-type's default), auto,
	// accept_edits or manual (accept-edits before 1.0; still accepted).
	PermissionMode string `json:"permission_mode"`
	// The target's policy (LOOM-89): purpose (personal, work or empty),
	// the only agent types allowed there (empty: all), and whether new
	// workspaces, shell commands and unconfirmed new work are allowed.
	Purpose             string   `json:"purpose"`
	AllowedAgentTypes   []string `json:"allowed_agent_types"`
	AllowProvision      bool     `json:"allow_provision"`
	AllowShell          bool     `json:"allow_shell"`
	RequireConfirmation bool     `json:"require_confirmation"`
	// Relay is what the router models may see of this target's work:
	// "full", "last_message", "none", or "" for its purpose's default;
	// RelayEffective is what applies (a work machine's default is none).
	Relay          string `json:"relay"`
	RelayEffective string `json:"relay_effective"`
	// SSHPort overrides the SSH config's port when non-zero (LOOM-114).
	SSHPort int `json:"ssh_port"`
	// PinnedHostKeys are the host keys pinned through the API (LOOM-114),
	// by type and fingerprint; empty when the target relies on the
	// mounted known_hosts.
	PinnedHostKeys []hostKeyResponse `json:"pinned_host_keys"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	// Health is the target's last health probe (LOOM-86); null when it
	// has never been probed, and in create/update responses.
	Health *targetHealthResponse `json:"health"`
}

// targetHealthResponse is a target's last health probe (LOOM-86).
type targetHealthResponse struct {
	// Status is "healthy" or "unhealthy"; Error says why when unhealthy.
	Status      string `json:"status"`
	Reachable   bool   `json:"reachable"`
	Error       string `json:"error,omitempty"`
	LatencyMS   int64  `json:"latency_ms"`
	TmuxVersion string `json:"tmux_version"`
	// DiskFreeBytes is free space on the workspace root's filesystem;
	// null when unknown.
	DiskFreeBytes *int64    `json:"disk_free_bytes"`
	LastProbedAt  time.Time `json:"last_probed_at"`
}

func newTargetHealthResponse(h *registry.TargetHealth) *targetHealthResponse {
	out := &targetHealthResponse{
		Status: "healthy", Reachable: h.Reachable, Error: h.Error, LatencyMS: h.Latency.Milliseconds(),
		TmuxVersion: h.TmuxVersion, LastProbedAt: h.ProbedAt,
	}
	if h.Error != "" {
		out.Status = "unhealthy"
	}
	if h.DiskFreeBytes >= 0 {
		free := h.DiskFreeBytes
		out.DiskFreeBytes = &free
	}
	return out
}

func newTargetResponse(t *registry.Target) targetResponse {
	return targetResponse{
		ID:                  t.ID,
		Name:                t.Name,
		Kind:                string(t.Kind),
		Host:                t.Host,
		User:                t.User,
		WorkspaceRoot:       t.WorkspaceRoot,
		PermissionMode:      apiEnum(t.PermissionMode),
		Purpose:             t.Policy.Purpose,
		AllowedAgentTypes:   append([]string{}, t.Policy.AllowedAgentTypes...),
		AllowProvision:      !t.Policy.NoProvision,
		AllowShell:          !t.Policy.NoShell,
		RequireConfirmation: t.Policy.RequireConfirmation,
		Relay:               t.Policy.Relay,
		RelayEffective:      t.Policy.EffectiveRelay(),
		SSHPort:             t.SSHPort,
		PinnedHostKeys:      pinnedHostKeys(t),
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
	}
}

// targetRequest is the create/update body. It deliberately has no id
// field: ids are server-minted (see handleCreateTarget), so a client
// that sends one has it ignored rather than silently honoured.
type targetRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	User string `json:"user"`
	// WorkspaceRoot is optional (LOOM-119): omitted on a PUT keeps what's
	// stored, an explicit "" clears it.
	WorkspaceRoot *string `json:"workspace_root"`
	// PermissionMode is optional the same way.
	PermissionMode *string `json:"permission_mode"`
	// The policy fields (LOOM-89) are optional the same way; on a create,
	// omitted means allowed.
	Purpose             *string   `json:"purpose"`
	AllowedAgentTypes   *[]string `json:"allowed_agent_types"`
	AllowProvision      *bool     `json:"allow_provision"`
	AllowShell          *bool     `json:"allow_shell"`
	RequireConfirmation *bool     `json:"require_confirmation"`
	// Relay is optional the same way: "" sets the purpose's default.
	Relay *string `json:"relay"`
	// SSHPort is optional the same way (LOOM-114); 0 means the SSH
	// config's port.
	SSHPort *int `json:"ssh_port"`
}

type listTargetsResponse struct {
	Targets []targetResponse `json:"targets"`
}

// decodeTargetRequest reads a create/update body into a registry.Target
// (ID left for the caller to set) and validates it with Target.Validate —
// the one validation path every entry point shares (LOOM-65). It writes
// the error response itself and reports whether the caller should
// continue.
func (s *Server) decodeTargetRequest(w http.ResponseWriter, r *http.Request, base *registry.Target) (*registry.Target, bool) {
	var req targetRequest
	if !readJSON(w, r, &req) {
		return nil, false
	}
	target := &registry.Target{
		Name: strings.TrimSpace(req.Name),
		Kind: registry.TargetKind(req.Kind),
		Host: req.Host,
		User: req.User,
	}
	// base is the stored target on an update: its optional fields stand
	// unless the request names them (LOOM-119).
	if base != nil {
		target.SSHKeyRef, target.WorkspaceRoot, target.PermissionMode = base.SSHKeyRef, base.WorkspaceRoot, base.PermissionMode
		target.Policy = base.Policy
		target.SSHPort = base.SSHPort
	}
	if req.SSHPort != nil {
		target.SSHPort = *req.SSHPort
	}
	if req.Purpose != nil {
		target.Policy.Purpose = *req.Purpose
	}
	if req.AllowedAgentTypes != nil {
		target.Policy.AllowedAgentTypes = *req.AllowedAgentTypes
		if len(target.Policy.AllowedAgentTypes) == 0 {
			target.Policy.AllowedAgentTypes = nil
		}
	}
	if req.AllowProvision != nil {
		target.Policy.NoProvision = !*req.AllowProvision
	}
	if req.AllowShell != nil {
		target.Policy.NoShell = !*req.AllowShell
	}
	if req.RequireConfirmation != nil {
		target.Policy.RequireConfirmation = *req.RequireConfirmation
	}
	if req.Relay != nil {
		target.Policy.Relay = *req.Relay
	}
	if req.WorkspaceRoot != nil {
		target.WorkspaceRoot = *req.WorkspaceRoot
	}
	if req.PermissionMode != nil {
		// accept_edits in the API; the legacy accept-edits is still taken
		// for 1.x. The store keeps its spelling.
		target.PermissionMode = strings.ReplaceAll(*req.PermissionMode, "_", "-")
	}
	if err := target.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if s.agentTypes != nil {
		for _, a := range target.Policy.AllowedAgentTypes {
			if !slices.Contains(s.agentTypes, a) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("allowed_agent_types: unknown agent type %q (known: %s)",
					a, strings.Join(s.agentTypes, ", ")))
				return nil, false
			}
		}
	}
	return target, true
}

// handleCreateTarget registers an execution target. The id is minted
// here rather than accepted from the client (as sessions already do):
// workspaces reference targets by id, and letting a caller choose one
// invites collisions and the hand-minted ids this endpoint exists to
// replace.
func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	target, ok := s.decodeTargetRequest(w, r, nil)
	if !ok {
		return
	}
	target.ID = uuid.NewString()
	if err := s.targets.CreateTarget(r.Context(), target); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			writeError(w, http.StatusConflict, "a target with that name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not create target")
		return
	}
	writeJSON(w, http.StatusCreated, newTargetResponse(target))
}

// handleListTargets is how a client discovers the server-assigned ids
// it needs to attach a workspace to a target.
func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.targets.ListTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list targets")
		return
	}
	healths, err := s.targets.ListTargetHealth(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read target health")
		return
	}
	healthByTarget := make(map[string]*registry.TargetHealth, len(healths))
	for _, h := range healths {
		healthByTarget[h.TargetID] = h
	}
	out := make([]targetResponse, 0, len(targets))
	for _, t := range targets {
		resp := newTargetResponse(t)
		if h := healthByTarget[t.ID]; h != nil {
			resp.Health = newTargetHealthResponse(h)
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, listTargetsResponse{Targets: out})
}

// handleUpdateTarget replaces a target's mutable fields wholesale. It is
// the only correction path once any workspace references the target,
// since DeleteTarget refuses with a conflict at that point.
//
// The stored row is re-read afterwards rather than returning the struct
// passed to UpdateTarget: that call fills in UpdatedAt but not
// CreatedAt, so only a read gives the client a canonical row.
func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.targets.GetTarget(r.Context(), id)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch target")
		return
	}
	target, ok := s.decodeTargetRequest(w, r, existing)
	if !ok {
		return
	}
	target.ID = id
	if err := s.targets.UpdateTarget(r.Context(), target); err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			writeError(w, http.StatusNotFound, "no such target")
		case errors.Is(err, registry.ErrConflict):
			writeError(w, http.StatusConflict, "a target with that name already exists")
		default:
			writeError(w, http.StatusInternalServerError, "could not update target")
		}
		return
	}

	stored, err := s.targets.GetTarget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read back updated target")
		return
	}
	writeJSON(w, http.StatusOK, newTargetResponse(stored))
}

// handleDeleteTarget removes a target that nothing references — undoing
// a mistaken registration. A target with workspaces attached is a
// conflict, not a 500: the store's foreign key is doing its job and the
// operator needs to be told which it is.
func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.targets.DeleteTarget(r.Context(), r.PathValue("id")); err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			writeError(w, http.StatusNotFound, "no such target")
		case errors.Is(err, registry.ErrConflict):
			writeError(w, http.StatusConflict, "target still has workspaces referencing it")
		default:
			writeError(w, http.StatusInternalServerError, "could not delete target")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// targetAgentResponse is one recorded agent CLI probe result (LOOM-71).
type targetAgentResponse struct {
	AgentType string `json:"agent_type"`
	Available bool   `json:"available"`
	// Path is the absolute path the CLI resolved to, Version what it
	// reported (LOOM-79); empty when unavailable.
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	// AuthStatus is logged_in, logged_out or unknown (LOOM-86); omitted
	// when the CLI wasn't asked.
	AuthStatus string    `json:"auth_status,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

type listTargetAgentsResponse struct {
	Agents []targetAgentResponse `json:"agents"`
}

func newListTargetAgentsResponse(agents []*registry.TargetAgent) listTargetAgentsResponse {
	out := make([]targetAgentResponse, 0, len(agents))
	for _, a := range agents {
		out = append(out, targetAgentResponse{
			AgentType: a.AgentType, Available: a.Available, Path: a.Path, Version: a.Version,
			AuthStatus: a.AuthStatus, CheckedAt: a.CheckedAt,
		})
	}
	return listTargetAgentsResponse{Agents: out}
}

// handleListTargetAgents returns which agent CLIs were found on a target
// the last time it was probed. An agent type never probed there is
// simply absent from the list.
func (s *Server) handleListTargetAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.targets.GetTarget(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such target")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read target")
		return
	}
	agents, err := s.targets.ListTargetAgents(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list target agents")
		return
	}
	writeJSON(w, http.StatusOK, newListTargetAgentsResponse(agents))
}

// handleProbeTarget probes the target now — health, then its agent CLIs
// if it answered — records and returns the results (LOOM-86). An
// unreachable target is a 200 whose health says so: the probe worked.
func (s *Server) handleProbeTarget(w http.ResponseWriter, r *http.Request) {
	if s.targetProber == nil {
		writeError(w, http.StatusNotImplemented, "target probing is not configured on this server")
		return
	}
	h, agents, err := s.targetProber.ProbeTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such target")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not probe target")
		return
	}
	writeJSON(w, http.StatusOK, probeTargetResponse{
		Health: newTargetHealthResponse(h),
		Agents: newListTargetAgentsResponse(agents).Agents,
	})
}

type probeTargetResponse struct {
	Health *targetHealthResponse `json:"health"`
	Agents []targetAgentResponse `json:"agents"`
}

type taskTurnResponse struct {
	ID          string `json:"id"`
	UserMessage string `json:"user_message"`
	// AgentMessage is the agent's own final message for the turn; empty
	// when its completion hook saved none (Pane is then all there is).
	AgentMessage string    `json:"agent_message"`
	Pane         string    `json:"pane"`
	CreatedAt    time.Time `json:"created_at"`
}

type taskTranscriptResponse struct {
	TaskID string             `json:"task_id"`
	Turns  []taskTurnResponse `json:"turns"`
	// HasMore says earlier turns remain; NextBefore is the before= value
	// that fetches them (LOOM-122).
	HasMore    bool   `json:"has_more"`
	NextBefore string `json:"next_before,omitempty"`
}

// Transcript page sizes (LOOM-122): a turn can carry a 256 KiB pane.
const (
	defaultTranscriptPage = 20
	maxTranscriptPage     = 100
)

// handleTaskTranscript returns what each turn of a task produced, oldest
// first (LOOM-91): the message sent, the agent's own final message and
// the pane's scrollback at the turn's end, credentials redacted. It is
// paged, latest turns first (LOOM-122): ?limit= (default 20, at most 100)
// and ?before=<turn id>, from the previous page's next_before.
func (s *Server) handleTaskTranscript(w http.ResponseWriter, r *http.Request) {
	if s.taskTurns == nil {
		writeError(w, http.StatusNotImplemented, "task transcripts are not configured on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.taskTurns.GetTask(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such task")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not fetch task")
		return
	}
	limit := defaultTranscriptPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxTranscriptPage {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", maxTranscriptPage))
			return
		}
		limit = n
	}
	turns, more, err := s.taskTurns.ListTaskTurnsPage(r.Context(), id, r.URL.Query().Get("before"), limit)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "before names no turn of this task")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list the task's turns")
		return
	}
	out := taskTranscriptResponse{TaskID: id, Turns: make([]taskTurnResponse, 0, len(turns)), HasMore: more}
	for _, t := range turns {
		out.Turns = append(out.Turns, taskTurnResponse{ID: t.ID, UserMessage: t.UserMessage, AgentMessage: t.AgentMessage,
			Pane: t.Pane, CreatedAt: t.CreatedAt})
	}
	if more && len(turns) > 0 {
		out.NextBefore = turns[0].ID
	}
	writeJSON(w, http.StatusOK, out)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// apiTaskStatus is a task status as the API spells it: snake_case, like
// the API's other enums ("awaiting_input", "needs_attention",
// "human_takeover"). The store keeps its own spelling (API v1 freeze
// review, item 9).
func apiTaskStatus(s registry.TaskStatus) string {
	return apiEnum(string(s))
}

// apiEnum spells a stored enum value the API's way: snake_case
// ("accept-edits" → "accept_edits"). Agent-type names such as
// "claude-code" are identifiers, not enums, and keep their spelling.
func apiEnum(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}
