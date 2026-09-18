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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/version"
)

// APIVersion is the server↔client API version this package serves
// (design spec §10 axis 1) — the "v1" in /api/v1/..., independent of
// version.Version (axis 4, the server's own release version).
const APIVersion = "v1"

// Dispatcher is the one operation Server needs from the domain layer —
// satisfied by *app.App. A narrow seam (this package doesn't import app
// directly) keeps its own tests fast and self-contained, mirroring
// router.RoutingModel / orchestrator.CompletionDetector's minimal-seam
// pattern elsewhere in this codebase.
type Dispatcher interface {
	Dispatch(ctx context.Context, conversationID, message string) (string, error)
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
// turns instead of only task-lifecycle rows (LOOM-31) — satisfied
// structurally by any registry.Store, mirroring TaskLister.
type MessageLister interface {
	ListMessagesByConversation(ctx context.Context, conversationID string) ([]*registry.Message, error)
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
	passwordHash       []byte
	sessionTTL         time.Duration
	loginThrottle      *loginThrottle
	streamPollInterval time.Duration
	staticDir          string
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

// NewServer constructs a Server. passwordHash is a bcrypt hash (see
// HashPassword) — the single v1 user's credential, verified at login;
// never a plaintext password.
func NewServer(dispatcher Dispatcher, sessions SessionStore, workspaces WorkspaceLister, tasks TaskLister, messages MessageLister, attachInfo AttachInfoStore, passwordHash []byte, opts ...Option) *Server {
	s := &Server{
		dispatcher:         dispatcher,
		sessions:           sessions,
		workspaces:         workspaces,
		tasks:              tasks,
		messages:           messages,
		attachInfo:         attachInfo,
		passwordHash:       passwordHash,
		sessionTTL:         defaultSessionTTL,
		loginThrottle:      newLoginThrottle(defaultLoginBackoffBase, defaultLoginBackoffMax),
		streamPollInterval: defaultStreamPollInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.staticDir != "" {
		s.static = newStaticHandler(s.staticDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/v1/sessions", s.requireAuth(s.handleListSessions))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.requireAuth(s.handleRevokeSession))
	mux.HandleFunc("POST /api/v1/dispatch", s.requireAuth(s.handleDispatch))
	mux.HandleFunc("GET /api/v1/workspaces", s.requireAuth(s.handleListWorkspaces))
	mux.HandleFunc("GET /api/v1/conversations", s.requireAuth(s.handleListConversations))
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.requireAuth(s.handleGetConversation))
	mux.HandleFunc("GET /api/v1/conversations/{id}/stream", s.requireAuth(s.handleStream))
	mux.HandleFunc("GET /api/v1/tasks/{id}/attach-info", s.requireAuth(s.handleAttachInfo))
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body")
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
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.sessions.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list sessions")
		return
	}
	current := sessionFromContext(r.Context())

	out := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
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

type dispatchRequest struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
}

type dispatchResponse struct {
	Reply string `json:"reply"`
}

func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req dispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ConversationID == "" || req.Message == "" {
		writeError(w, http.StatusBadRequest, "conversation_id and message are required")
		return
	}

	reply, err := s.dispatcher.Dispatch(r.Context(), req.ConversationID, req.Message)
	if err != nil {
		// Surfaced verbatim, not genericized: design spec's error-handling
		// section requires routing failures to reach the user as an
		// explicit message, never a silently dropped one — and this
		// endpoint is auth-gated, so the exposure is to the authenticated
		// owner only, not an anonymous caller.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dispatchResponse{Reply: reply})
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
	IsDynamic      bool       `json:"is_dynamic"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

type listWorkspacesResponse struct {
	Workspaces []workspaceSummary `json:"workspaces"`
}

// handleListWorkspaces returns a summary of every registered workspace
// (id/name/target/status plus tags/description/capabilities/rolling_summary/
// is_dynamic/last_used_at), sorted by name (registry.Store's own
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
			IsDynamic:      ws.IsDynamic,
			LastUsedAt:     ws.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, listWorkspacesResponse{Workspaces: out})
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
}

type listConversationsResponse struct {
	Conversations []conversationSummary `json:"conversations"`
}

// handleListConversations returns one summary row per distinct
// conversation_id, sorted most-recently-updated first — the "which
// conversation is active" overview. A conversation isn't a stored entity
// (design spec has no such table); this groups TaskLister.ListTasks by
// ConversationID, taking each conversation's most-recently-updated task
// as representative, since the same conversation can span more than one
// task (LOOM-13 continuation, or a later message routed to a different
// workspace by the router).
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list conversations")
		return
	}

	latest := make(map[string]*registry.Task, len(tasks))
	for _, t := range tasks {
		cur, ok := latest[t.ConversationID]
		if !ok || t.UpdatedAt.After(cur.UpdatedAt) {
			latest[t.ConversationID] = t
		}
	}

	out := make([]conversationSummary, 0, len(latest))
	for convID, t := range latest {
		out = append(out, conversationSummary{
			ConversationID: convID,
			WorkspaceID:    t.WorkspaceID,
			Status:         string(t.Status),
			UpdatedAt:      t.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })

	writeJSON(w, http.StatusOK, listConversationsResponse{Conversations: out})
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
}

type messageSummary struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TaskID    string    `json:"task_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type getConversationResponse struct {
	ConversationID string             `json:"conversation_id"`
	Tasks          []conversationTask `json:"tasks"`
	Messages       []messageSummary   `json:"messages"`
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
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

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
			ID:          t.ID,
			WorkspaceID: t.WorkspaceID,
			Kind:        string(t.Kind),
			AgentType:   t.AgentType,
			Status:      string(t.Status),
			CreatedAt:   t.CreatedAt,
			UpdatedAt:   t.UpdatedAt,
			StartedAt:   t.StartedAt,
			CompletedAt: t.CompletedAt,
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
			ID:        m.ID,
			Role:      string(m.Role),
			Content:   m.Content,
			TaskID:    m.TaskID,
			CreatedAt: m.CreatedAt,
		})
	}

	// A conversation is only truly unknown if it has neither task
	// history nor any logged messages — an answer_directly-only
	// conversation (LOOM-31) has messages but zero tasks, and must not
	// 404.
	if len(out) == 0 && len(msgOut) == 0 {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	}

	writeJSON(w, http.StatusOK, getConversationResponse{ConversationID: id, Tasks: out, Messages: msgOut})
}

type attachTargetInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	User string `json:"user"`
}

type attachInfoResponse struct {
	TaskID      string           `json:"task_id"`
	TmuxSession string           `json:"tmux_session"`
	Target      attachTargetInfo `json:"target"`
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
		TaskID:      task.ID,
		TmuxSession: task.TmuxSession,
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
}

// handleStream serves GET /api/v1/conversations/{id}/stream (LOOM-21):
// Server-Sent Events reporting task status transitions for one
// conversation, so a client can watch a dispatch progress instead of
// only getting a single reply when POST /dispatch's blocking call
// eventually returns (that endpoint's contract is unchanged — this is
// purely additive). No 404 for an unknown conversation_id: a client may
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
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-poll.C:
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
		TaskID:      latest.ID,
		WorkspaceID: latest.WorkspaceID,
		Status:      string(latest.Status),
		UpdatedAt:   latest.UpdatedAt,
	}, nil
}

type versionResponse struct {
	ServerVersion string `json:"server_version"`
	APIVersion    string `json:"api_version"`
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
func newStaticHandler(dir string) http.HandlerFunc {
	root := http.Dir(dir)
	fileServer := http.FileServer(root)
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
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
		http.ServeContent(w, r, "index.html", info.ModTime(), f)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := root.Open(r.URL.Path)
		if err != nil {
			serveIndex(w, r)
			return
		}
		info, statErr := f.Stat()
		f.Close()
		if statErr != nil || info.IsDir() {
			serveIndex(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
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
