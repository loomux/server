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

// maxDispatchMessage is the longest message POST /dispatch takes
// (LOOM-155). The router refuses to send an agent more than
// targets.MaxPasteBytes with its context, so a message much over that
// can only fail, and refusing it here keeps it from reaching the router
// model first. The margin leaves room for a message answered directly,
// which never goes to an agent.
const maxDispatchMessage = targets.MaxPasteBytes + 4<<10

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

// invalidRequestText is a dispatch.ErrInvalidRequest's reason without
// the package's prefix, e.g. "message is required".
func invalidRequestText(err error) string {
	return strings.TrimPrefix(err.Error(), dispatch.ErrInvalidRequest.Error()+": ")
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
	dreq := dispatch.Request{
		ConversationID: req.ConversationID,
		Message:        req.Message,
		WorkspaceHint:  req.WorkspaceHint,
		ConfirmationID: req.ConfirmationID,
		IdempotencyKey: key,
	}
	// A conversation_id is a path segment everywhere else in the API, so
	// it must be one that can be fetched back (LOOM-154).
	if err := dreq.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, invalidRequestText(err))
		return
	}
	if len(req.Message) > maxDispatchMessage {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"the message is %d KiB, over the %d KiB Loomux takes. Put the long part in a file in the workspace and ask the agent to read it",
			(len(req.Message)+1023)/1024, maxDispatchMessage>>10))
		return
	}

	d, err := s.dispatcher.Submit(r.Context(), dreq)
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
		writeError(w, http.StatusBadRequest, invalidRequestText(err))
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

	// The wait ends with its session (LOOM-182), as a stream does
	// (LOOM-144), through the same mechanism: a turn that finishes after
	// logout or a revoke must not hand its reply to the token that was
	// revoked. The session is checked again once the wait is registered,
	// for a revoke that landed between requireAuth and here.
	sess := sessionFromContext(r.Context())
	waitCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer s.waits.add(sess.ID, cancel)()
	if !s.sessionStillValid(waitCtx, sess) {
		cancel()
	}
	done, err := s.dispatcher.Wait(waitCtx, d.ID)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client is gone; the job carries on without it
		}
		if waitCtx.Err() != nil {
			// The session ended; the job carries on, its result in the
			// conversation for whoever signs in next.
			writeError(w, http.StatusUnauthorized, "invalid or expired session")
			return
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

// dispatchCursor is what a stream knows of its conversation's jobs:
// each one's last reported state, and the newest read, after which the
// next poll reads new jobs (LOOM-145). Jobs not finished yet are reread
// by id, since only they can still change.
type dispatchCursor struct {
	seen map[string]dispatchUpdateEvent
	last string
}

// sendDispatchUpdates writes a dispatch_update for every job of the
// conversation that changed since seen, and returns the updated seen. A
// nil seen is the baseline, taken at connect (LOOM-137): finished jobs
// are only recorded, jobs still in flight are reported.
func (s *Server) sendDispatchUpdates(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	conversationID string, cur *dispatchCursor) *dispatchCursor {
	var dispatches []*registry.Dispatch
	var err error
	unfinished := map[string]bool{}
	if s.streamQ != nil {
		after := ""
		var also []string
		if cur != nil {
			after = cur.last
			for id, ev := range cur.seen {
				if !registry.DispatchStatus(ev.Status).Terminal() {
					also = append(also, id)
					unfinished[id] = true
				}
			}
		}
		dispatches, err = s.streamQ.ListDispatchesAfter(ctx, conversationID, after, also)
	} else {
		dispatches, err = s.dispatcher.ListByConversation(ctx, conversationID)
	}
	if err != nil {
		return cur
	}
	first := cur == nil
	if first {
		cur = &dispatchCursor{seen: make(map[string]dispatchUpdateEvent, len(dispatches))}
	}
	for _, d := range dispatches {
		if !unfinished[d.ID] {
			cur.last = d.ID // a reread job is older than the cursor
		}
		ev := dispatchUpdateEvent{
			DispatchID: d.ID, Status: string(d.Status), Reply: d.Reply,
			Error: d.Error, ErrorClass: string(d.ErrorClass), UpdatedAt: d.UpdatedAt,
		}
		if prev, ok := cur.seen[d.ID]; ok && prev.Status == ev.Status && prev.UpdatedAt.Equal(ev.UpdatedAt) {
			continue
		}
		cur.seen[d.ID] = ev
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
	return cur
}
