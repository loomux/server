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
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
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

// maxDispatchMessageBytes is the largest message POST /dispatch accepts
// (LOOM-155). The router refuses anything over targets.MaxPasteBytes with
// its context (error class message_too_large), but only after a routing
// call; a message that alone is well past it can never be sent, so it is
// refused here before any routing or provisioning. The margin leaves the
// borderline case, where only the added context tips it over, to the
// router's own check and its error class.
const maxDispatchMessageBytes = targets.MaxPasteBytes + 1<<10

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
	// ConfirmationID (LOOM-123) is set by an offer's Approve or Deny: the
	// message answers that offer, and runs nothing if it's no longer the
	// one awaiting an answer.
	ConfirmationID string `json:"confirmation_id,omitempty"`
}

// dispatchResponse is a dispatch job as clients see it: the body of
// GET /dispatches/{id}, of POST /dispatch (async, or blocking once
// finished), and an entry of a conversation's dispatches.
type dispatchResponse struct {
	DispatchID     string `json:"dispatch_id"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Reply          string `json:"reply,omitempty"`
	Error          string `json:"error,omitempty"`
	ErrorClass     string `json:"error_class,omitempty"`
	// ConfirmationID is the offer this message answered (LOOM-123), so a
	// client retrying a failed answer can send it again with the same id
	// and the server refuses it if that offer is no longer open, instead
	// of a bare "yes" answering whichever offer is pending by then.
	ConfirmationID string     `json:"confirmation_id,omitempty"`
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
		ConfirmationID: d.ConfirmationID,
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

// parseDispatchMode reads ?wait=true (blocking) and
// `Prefer: respond-async` (async). Neither means dispatchAsyncByDefault.
// ?async=true was dropped before 1.0 (API v1 freeze review, item 4): it
// duplicated the header and the default, and is now ignored.
func parseDispatchMode(r *http.Request) (dispatchMode, error) {
	q := r.URL.Query()
	wait := q.Get("wait") == "true"
	preferAsync := preferRespondAsync(r.Header.Values("Prefer"))
	async := preferAsync
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
// busyResponse is POST /api/v1/dispatch's 409 when the conversation
// already has a dispatch running: the error body plus that dispatch's id,
// so the client can follow it instead of retrying blind.
type busyResponse struct {
	errorResponse
	DispatchID string `json:"dispatch_id"`
}

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
	if !readJSON(w, r, &req) {
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(req.Message) > maxDispatchMessageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"message is %d KiB, over the %d KiB Loomux sends to an agent. "+
				"Put the long part in a file in the workspace and ask the agent to read it",
			(len(req.Message)+1023)/1024, targets.MaxPasteBytes>>10))
		return
	}

	d, err := s.dispatcher.Submit(r.Context(), dispatch.Request{
		ConversationID: req.ConversationID,
		Message:        req.Message,
		WorkspaceHint:  req.WorkspaceHint,
		ConfirmationID: req.ConfirmationID,
		IdempotencyKey: key,
	})
	var busy *dispatch.BusyError
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, busyResponse{
			errorResponse: errorResponse{Error: err.Error(), Code: codeConversationBusy},
			DispatchID:    busy.DispatchID,
		})
		return
	case errors.Is(err, dispatch.ErrKeyReused):
		writeErrorCode(w, http.StatusUnprocessableEntity, codeIdempotencyConflict, err.Error())
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

// handleCancelDispatch stops a running turn (LOOM-99). 202: the job has
// been told; it ends failed with class cancelled, which the stream and
// GET /dispatches/{id} report.
func (s *Server) handleCancelDispatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch err := s.dispatcher.Cancel(r.Context(), id); {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such dispatch")
	case errors.Is(err, dispatch.ErrNotRunning):
		writeError(w, http.StatusConflict, "the dispatch isn't running")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not cancel dispatch")
	default:
		writeJSON(w, http.StatusAccepted, cancelResponse{DispatchID: id})
	}
}

type cancelResponse struct {
	TaskID     string `json:"task_id,omitempty"`
	DispatchID string `json:"dispatch_id,omitempty"`
}

// handleCancelTask stops a running task (LOOM-99): its agent is
// interrupted and the task failed with class cancelled, the pane kept.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	if s.taskCanceller == nil {
		writeError(w, http.StatusNotImplemented, "task cancellation is not configured on this server")
		return
	}
	id := r.PathValue("id")
	dispatchID, err := s.taskCanceller.CancelTask(r.Context(), id)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such task")
	case errors.Is(err, orchestrator.ErrTaskInactive):
		writeError(w, http.StatusConflict, "the task has already ended")
	case errors.Is(err, orchestrator.ErrHumanTakeover):
		writeError(w, http.StatusConflict, "someone has taken over this task; release it first")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not cancel task")
	default:
		writeJSON(w, http.StatusAccepted, cancelResponse{TaskID: id, DispatchID: dispatchID})
	}
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
// nil seen is the baseline, taken at connect (LOOM-137): finished jobs
// are only recorded, jobs still in flight are reported.
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
