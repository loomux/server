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
	"net/http"
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
// (including *app.App.Store()).
type SessionStore interface {
	CreateSession(ctx context.Context, s *registry.Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*registry.Session, error)
	TouchSession(ctx context.Context, id string, lastUsedAt time.Time) error
	DeleteSession(ctx context.Context, id string) error
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

// Server is the client-facing HTTP API. It implements http.Handler —
// callers decide how to actually listen (http.ListenAndServe,
// httptest.Server, etc.), keeping this package transport-agnostic about
// TLS/networking (deployment assumption: plain HTTP behind a reverse
// proxy that terminates TLS — see README.md).
type Server struct {
	dispatcher    Dispatcher
	sessions      SessionStore
	passwordHash  []byte
	sessionTTL    time.Duration
	loginThrottle *loginThrottle
	mux           *http.ServeMux
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

// NewServer constructs a Server. passwordHash is a bcrypt hash (see
// HashPassword) — the single v1 user's credential, verified at login;
// never a plaintext password.
func NewServer(dispatcher Dispatcher, sessions SessionStore, passwordHash []byte, opts ...Option) *Server {
	s := &Server{
		dispatcher:    dispatcher,
		sessions:      sessions,
		passwordHash:  passwordHash,
		sessionTTL:    defaultSessionTTL,
		loginThrottle: newLoginThrottle(defaultLoginBackoffBase, defaultLoginBackoffMax),
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("POST /api/v1/dispatch", s.requireAuth(s.handleDispatch))
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
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		s.handleUnsupportedAPIPath(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
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
