package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Loomux/server/registry"
)

// sessionStreams tracks each session's open event streams, so logging out
// or revoking a session ends them at once (LOOM-144): requireAuth runs
// only when a stream connects, and a stream otherwise outlives its
// session for as long as the client keeps it open.
type sessionStreams struct {
	mu      sync.Mutex
	streams map[string]map[*context.CancelFunc]struct{}
	// closed is set by endAll: a stream registering after it (one that
	// was still in auth when shutdown began) is ended at once.
	closed bool
}

// add registers cancel under sessionID; the returned func unregisters it.
func (s *sessionStreams) add(sessionID string, cancel context.CancelFunc) (remove func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		cancel()
		return func() {}
	}
	if s.streams == nil {
		s.streams = map[string]map[*context.CancelFunc]struct{}{}
	}
	if s.streams[sessionID] == nil {
		s.streams[sessionID] = map[*context.CancelFunc]struct{}{}
	}
	key := &cancel
	s.streams[sessionID][key] = struct{}{}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.streams[sessionID], key)
		if len(s.streams[sessionID]) == 0 {
			delete(s.streams, sessionID)
		}
	}
}

// end cancels every open stream of sessionID. It calls the cancels under
// s.mu: safe because a plain WithCancel cancel runs no callbacks (no
// context.AfterFunc is registered on these contexts) and so can't come
// back for the lock.
func (s *sessionStreams) end(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cancel := range s.streams[sessionID] {
		(*cancel)()
	}
}

// endAll cancels every open stream, of every session.
func (s *sessionStreams) endAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, set := range s.streams {
		for cancel := range set {
			(*cancel)()
		}
	}
}

// EndStreams ends every open conversation stream (LOOM-147). A stream
// never goes idle on its own, so an http.Server shutdown would otherwise
// wait out its whole deadline for them: register this with
// http.Server.RegisterOnShutdown. Clients see the stream end and
// reconnect to whichever server is up next.
func (s *Server) EndStreams() { s.streams.endAll() }

// sessionStillValid re-checks a stream's session, for the case end can't
// see: a session that expired, or was deleted other than through this
// API, while its stream stayed open.
func (s *Server) sessionStillValid(ctx context.Context, sess *registry.Session) bool {
	cur, err := s.sessions.GetSessionByTokenHash(ctx, sess.TokenHash)
	if err != nil {
		// Only a session that is really gone ends the stream; a passing
		// database error is retried at the next heartbeat.
		return !errors.Is(err, registry.ErrNotFound)
	}
	return time.Since(cur.LastUsedAt) <= s.sessionTTL
}
