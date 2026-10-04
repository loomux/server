package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
)

// dispatchAsyncByDefault is the LOOM-80 rollout switch: what POST
// /dispatch does when the client asks for neither mode. It was false while
// the deployed web still expected a blocking {reply}; the LOOM-81 web
// (deploy/web-ref d14a402) asks for async itself, so async is now the
// default and a client wanting the reply in the response asks with
// ?wait=true. Both modes run the turn as a job on a server-owned
// context; this only decides whether the response waits.
const dispatchAsyncByDefault = true

// maxIdempotencyKeyLen bounds the Idempotency-Key header.
const maxIdempotencyKeyLen = 255

type dispatchRequest struct {
	// ConversationID is optional (LOOM-80): empty starts a new
	// conversation, whose id the response returns.
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	// WorkspaceHint (LOOM-46) is optional: a client-supplied workspace
	// ID the caller believes this message likely belongs to (e.g. a
	// chat UI already focused on that workspace's conversation). It's
	// advisory only — see router.WithWorkspaceHint.
	WorkspaceHint string `json:"workspace_hint,omitempty"`
}

// dispatchResponse is a dispatch job as clients see it: the body of
// GET /dispatches/{id}, of POST /dispatch (async, or blocking once
// finished), and an entry of a conversation's dispatches.
type dispatchResponse struct {
	DispatchID     string     `json:"dispatch_id"`
	ConversationID string     `json:"conversation_id"`
	Status         string     `json:"status"`
	Reply          string     `json:"reply,omitempty"`
	Error          string     `json:"error,omitempty"`
	ErrorClass     string     `json:"error_class,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

func newDispatchResponse(d *registry.Dispatch) dispatchResponse {
	return dispatchResponse{
		DispatchID:     d.ID,
		ConversationID: d.ConversationID,
		Status:         string(d.Status),
		Reply:          d.Reply,
		Error:          d.Error,
		ErrorClass:     string(d.ErrorClass),
		CreatedAt:      d.CreatedAt,
		StartedAt:      d.StartedAt,
		FinishedAt:     d.FinishedAt,
	}
}

// dispatchMode is how a POST /dispatch wants its answer.
type dispatchMode struct {
	async bool
	// preferHeader is set when async was asked for with Prefer, so the
	// response can say it was applied (RFC 7240).
	preferHeader bool
}

// parseDispatchMode reads ?wait=true (blocking), and ?async=true or
// `Prefer: respond-async` (async). Neither means dispatchAsyncByDefault.
func parseDispatchMode(r *http.Request) (dispatchMode, error) {
	q := r.URL.Query()
	wait := q.Get("wait") == "true"
	preferAsync := preferRespondAsync(r.Header.Values("Prefer"))
	async := q.Get("async") == "true" || preferAsync
	if wait && async {
		return dispatchMode{}, errors.New("wait=true can't be combined with an async request")
	}
	if !wait && !async {
		return dispatchMode{async: dispatchAsyncByDefault}, nil
	}
	return dispatchMode{async: async, preferHeader: preferAsync}, nil
}

func preferRespondAsync(values []string) bool {
	for _, v := range values {
		for _, pref := range strings.Split(v, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(pref), ";")
			if strings.EqualFold(strings.TrimSpace(name), "respond-async") {
				return true
			}
		}
	}
	return false
}

// handleDispatch serves POST /api/v1/dispatch (LOOM-80). Every request
// becomes a dispatch job — or, by Idempotency-Key, finds the one it
// already made — so the turn never runs on the request's context:
// a client that goes away mid-turn loses nothing, and the result is in
// the job and the conversation's history. Async mode answers 202 at once;
// blocking mode waits for the job and answers as this endpoint always
// has, {reply} or a 500 with the error.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	mode, err := parseDispatchMode(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Idempotency-Key must be at most %d characters", maxIdempotencyKeyLen))
		return
	}
	var req dispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	d, err := s.dispatcher.Submit(r.Context(), dispatch.Request{
		ConversationID: req.ConversationID,
		Message:        req.Message,
		WorkspaceHint:  req.WorkspaceHint,
		IdempotencyKey: key,
	})
	var busy *dispatch.BusyError
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "dispatch_id": busy.DispatchID})
		return
	case errors.Is(err, dispatch.ErrKeyReused):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, dispatch.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, dispatch.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not start dispatch")
		return
	}

	if mode.async {
		w.Header().Set("Location", "/api/v1/dispatches/"+d.ID)
		if mode.preferHeader {
			w.Header().Set("Preference-Applied", "respond-async")
		}
		writeJSON(w, http.StatusAccepted, newDispatchResponse(d))
		return
	}

	done, err := s.dispatcher.Wait(r.Context(), d.ID)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client is gone; the job carries on without it
		}
		writeError(w, http.StatusInternalServerError, "could not wait for dispatch")
		return
	}
	if done.Status != registry.DispatchStatusSucceeded {
		// Surfaced verbatim, not genericized: design spec's error-handling
		// section requires routing failures to reach the user as an
		// explicit message, never a silently dropped one — and this
		// endpoint is auth-gated, so the exposure is to the authenticated
		// owner only, not an anonymous caller.
		writeJSON(w, http.StatusInternalServerError, newDispatchResponse(done))
		return
	}
	writeJSON(w, http.StatusOK, newDispatchResponse(done))
}

// handleGetDispatch serves GET /api/v1/dispatches/{id} (LOOM-80).
func (s *Server) handleGetDispatch(w http.ResponseWriter, r *http.Request) {
	d, err := s.dispatcher.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such dispatch")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch dispatch")
		return
	}
	writeJSON(w, http.StatusOK, newDispatchResponse(d))
}

// dispatchUpdateEvent is the data of a stream's `event: dispatch_update`.
type dispatchUpdateEvent struct {
	DispatchID string    `json:"dispatch_id"`
	Status     string    `json:"status"`
	Reply      string    `json:"reply,omitempty"`
	Error      string    `json:"error,omitempty"`
	ErrorClass string    `json:"error_class,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// sendDispatchUpdates writes a dispatch_update for every job of the
// conversation that changed since seen, and returns the updated seen. A
// nil seen is the stream's first poll: finished jobs are only recorded,
// jobs still in flight are reported.
func (s *Server) sendDispatchUpdates(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	conversationID string, seen map[string]dispatchUpdateEvent) map[string]dispatchUpdateEvent {
	dispatches, err := s.dispatcher.ListByConversation(ctx, conversationID)
	if err != nil {
		return seen
	}
	first := seen == nil
	if first {
		seen = make(map[string]dispatchUpdateEvent, len(dispatches))
	}
	for _, d := range dispatches {
		ev := dispatchUpdateEvent{
			DispatchID: d.ID, Status: string(d.Status), Reply: d.Reply,
			Error: d.Error, ErrorClass: string(d.ErrorClass), UpdatedAt: d.UpdatedAt,
		}
		if prev, ok := seen[d.ID]; ok && prev.Status == ev.Status && prev.UpdatedAt.Equal(ev.UpdatedAt) {
			continue
		}
		seen[d.ID] = ev
		if first && d.Status.Terminal() {
			continue
		}
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: dispatch_update\ndata: %s\n\n", data)
		flusher.Flush()
	}
	return seen
}
