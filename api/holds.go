package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// TaskHolds takes, renews and releases holds on tasks (LOOM-264,
// docs/design/tui-client-design.md "Server awareness: holds"): the lease
// a client takes while a person is attached to a task's pane. Satisfied
// by *orchestrator.Orchestrator. Optional: with none configured
// (WithTaskHolds), the takeover, heartbeat and release endpoints answer
// 501.
type TaskHolds interface {
	Takeover(ctx context.Context, req orchestrator.TakeoverRequest) (*orchestrator.TakeoverResult, error)
	Heartbeat(ctx context.Context, taskID, holdID, sessionID string) (*registry.TaskHold, error)
	Release(ctx context.Context, taskID, holdID string) (*registry.HoldRelease, error)
}

// WithTaskHolds enables POST /api/v1/tasks/{id}/takeover,
// /takeover/heartbeat and /release (LOOM-264).
func WithTaskHolds(h TaskHolds) Option {
	return func(s *Server) { s.taskHolds = h }
}

// Error codes of the hold endpoints. See api/README.md, "Errors".
const (
	// codeTaskHeld: takeover's 409 when another session holds the task
	// (the body's hold says who); force takes it from them.
	codeTaskHeld = "task_held"
	// codeTaskInactive: takeover's 409 for a task that has ended and
	// whose pane wasn't kept for a person.
	codeTaskInactive = "task_inactive"
	// codeUnsupportedMode: takeover's 400 for a mode this server doesn't
	// serve (watch, until watch holds exist).
	codeUnsupportedMode = "unsupported_mode"
	// codeHoldEnded: heartbeat's 410 for a hold that has ended (the
	// body's ended_reason says why).
	codeHoldEnded = "hold_ended"
)

// maxClientLabel bounds a hold's client_label, in characters.
const maxClientLabel = 64

// holdResponse is a registry.TaskHold as clients see it.
type holdResponse struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	// SessionID is the session holding the task, as GET /sessions names
	// it. ClientKind and ClientLabel describe that client ("tui",
	// "laptop"); either may be empty.
	SessionID   string `json:"session_id"`
	ClientKind  string `json:"client_kind"`
	ClientLabel string `json:"client_label"`
	// Mode is drive or watch.
	Mode string `json:"mode"`
	// PrevStatus is the task's status when the hold was taken.
	PrevStatus string `json:"prev_status"`
	// TTLSeconds is the lease's length: a heartbeat moves expires_at to
	// that long after it.
	TTLSeconds int       `json:"ttl_seconds"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	// EndedAt and EndedReason are set once the hold has ended: released,
	// expired, taken_back, taken_over, session_revoked or task_ended.
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedReason string     `json:"ended_reason,omitempty"`
}

func newHoldResponse(h *registry.TaskHold) *holdResponse {
	if h == nil {
		return nil
	}
	return &holdResponse{
		ID:          h.ID,
		TaskID:      h.TaskID,
		SessionID:   h.SessionID,
		ClientKind:  h.ClientKind,
		ClientLabel: h.ClientLabel,
		Mode:        string(h.Mode),
		PrevStatus:  apiTaskStatus(h.PrevStatus),
		TTLSeconds:  int(h.TTL / time.Second),
		AcquiredAt:  h.AcquiredAt,
		RenewedAt:   h.RenewedAt,
		ExpiresAt:   h.ExpiresAt,
		EndedAt:     h.EndedAt,
		EndedReason: string(h.EndedReason),
	}
}

type takeoverRequest struct {
	// Mode is drive (the default) or watch.
	Mode string `json:"mode,omitempty"`
	// ClientLabel names this client to the others ("laptop").
	ClientLabel string `json:"client_label,omitempty"`
	// TTLSeconds is the lease's length, 15 to 300; 45 when left out.
	TTLSeconds *int `json:"ttl_seconds,omitempty"`
	// Force takes the task from another session that holds it.
	Force bool `json:"force,omitempty"`
}

// takeoverResponse is the hold and how to attach, in one round trip.
type takeoverResponse struct {
	TaskID string             `json:"task_id"`
	Hold   *holdResponse      `json:"hold"`
	Attach attachInfoResponse `json:"attach"`
}

// taskHeldResponse is takeover's 409 task_held: the error envelope plus
// the hold that has the task.
type taskHeldResponse struct {
	Error string        `json:"error"`
	Code  string        `json:"code"`
	Hold  *holdResponse `json:"hold"`
}

type heartbeatRequest struct {
	HoldID string `json:"hold_id"`
}

type heartbeatResponse struct {
	Hold *holdResponse `json:"hold"`
}

// holdEndedResponse is heartbeat's 410 hold_ended: the error envelope
// plus why the hold ended, and the hold.
type holdEndedResponse struct {
	Error       string        `json:"error"`
	Code        string        `json:"code"`
	EndedReason string        `json:"ended_reason"`
	Hold        *holdResponse `json:"hold"`
}

type releaseRequest struct {
	// HoldID is the hold to release. Left out, it is Take back: whatever
	// drive hold the task has is ended.
	HoldID string `json:"hold_id,omitempty"`
}

type releaseResponse struct {
	TaskID string `json:"task_id"`
	// Status is the task's status after the release.
	Status string `json:"status"`
	// Hold is the hold released, or the one found already ended; absent
	// when none was named and the task had none.
	Hold *holdResponse `json:"hold,omitempty"`
}

// readOptionalJSON is readJSON for a body that may be left out: an empty
// one, or one of only whitespace, leaves v as it is. A body sent chunked
// has no length to tell by, so it is read to its first byte that isn't
// whitespace, under readJSON's bounds. Only a body that ends there is
// none: one too big, cut off or stalled is refused as readJSON would,
// because what it went on to say isn't known.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if r.ContentLength > 0 {
		return readJSON(w, r, v)
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxRequestBody))
	for {
		c, err := body.ReadByte()
		if err == io.EOF {
			// Nothing but whitespace (or nothing at all): no body.
			_ = rc.SetReadDeadline(time.Time{})
			return true
		}
		if err != nil {
			return refuseBody(w, body, err)
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			_ = body.UnreadByte()
			break
		}
	}
	return decodeBody(w, rc, body, v)
}

// cleanClientLabel trims label and reports whether it may be shown to
// other clients as is: short, and no control characters.
func cleanClientLabel(label string) (string, bool) {
	label = strings.TrimSpace(label)
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > maxClientLabel {
		return "", false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return label, true
}

// handleTakeover serves POST /api/v1/tasks/{id}/takeover (LOOM-264): the
// caller's session takes the drive hold on the task, which pauses
// automation into its pane, and gets the attach-info with it. The hold is
// checked before the task's status: the session's own hold is renewed
// and returned, another session's is a 409 task_held unless force. See
// orchestrator.Takeover.
func (s *Server) handleTakeover(w http.ResponseWriter, r *http.Request) {
	if s.taskHolds == nil {
		writeError(w, http.StatusNotImplemented, "task takeover is not configured on this server")
		return
	}
	var req takeoverRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	switch registry.HoldMode(req.Mode) {
	case "", registry.HoldModeDrive:
	case registry.HoldModeWatch:
		// Refused rather than answered with a drive hold: a client that
		// asked only to look must never be handed the pane.
		writeErrorCode(w, http.StatusBadRequest, codeUnsupportedMode, "this server doesn't support watch holds yet")
		return
	default:
		writeError(w, http.StatusBadRequest, `mode must be "drive" or "watch"`)
		return
	}
	var ttl time.Duration
	if req.TTLSeconds != nil {
		// Compared as seconds: a huge value would wrap as a Duration.
		if *req.TTLSeconds < int(orchestrator.MinHoldTTL/time.Second) || *req.TTLSeconds > int(orchestrator.MaxHoldTTL/time.Second) {
			writeError(w, http.StatusBadRequest, "ttl_seconds must be 15 to 300")
			return
		}
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	label, ok := cleanClientLabel(req.ClientLabel)
	if !ok {
		writeError(w, http.StatusBadRequest, "client_label must be at most 64 characters, with no control characters")
		return
	}

	id := r.PathValue("id")
	// Before the hold is taken: an unknown task is a 404 here, and a
	// takeover never succeeds without its attach-info.
	attach, ok := s.attachInfoFor(w, r, id)
	if !ok {
		return
	}
	res, err := s.taskHolds.Takeover(r.Context(), orchestrator.TakeoverRequest{
		TaskID:      id,
		SessionID:   sessionFromContext(r.Context()).ID,
		ClientLabel: label,
		Mode:        registry.HoldModeDrive,
		TTL:         ttl,
		Force:       req.Force,
	})
	var held *orchestrator.TaskHeldError
	var invalid *orchestrator.InvalidTakeoverError
	switch {
	case errors.As(err, &held):
		writeJSON(w, http.StatusConflict, taskHeldResponse{
			Error: "another session holds this task; take it with force, or take it back",
			Code:  codeTaskHeld,
			Hold:  newHoldResponse(held.Hold),
		})
	case errors.Is(err, orchestrator.ErrTaskInactive):
		writeErrorCode(w, http.StatusConflict, codeTaskInactive, "the task has ended and its pane wasn't kept for a person")
	case errors.Is(err, orchestrator.ErrUnsupportedHoldMode):
		writeErrorCode(w, http.StatusBadRequest, codeUnsupportedMode, "this server doesn't support that hold mode yet")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such task")
	case errors.Is(err, orchestrator.ErrStatusChanged):
		writeError(w, http.StatusConflict, "the task kept changing while it was being taken over; look at it again")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not take over task")
	default:
		writeJSON(w, http.StatusOK, takeoverResponse{TaskID: id, Hold: newHoldResponse(res.Hold), Attach: attach})
	}
}

// handleHoldHeartbeat serves POST /api/v1/tasks/{id}/takeover/heartbeat
// (LOOM-264): the holder renews its lease. An ended hold is 410
// hold_ended with why.
func (s *Server) handleHoldHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.taskHolds == nil {
		writeError(w, http.StatusNotImplemented, "task takeover is not configured on this server")
		return
	}
	var req heartbeatRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.HoldID == "" {
		writeError(w, http.StatusBadRequest, "hold_id is required")
		return
	}
	hold, err := s.taskHolds.Heartbeat(r.Context(), r.PathValue("id"), req.HoldID, sessionFromContext(r.Context()).ID)
	var ended *orchestrator.HoldEndedError
	switch {
	case errors.As(err, &ended):
		writeJSON(w, http.StatusGone, holdEndedResponse{
			Error:       "the hold has ended",
			Code:        codeHoldEnded,
			EndedReason: string(ended.Hold.EndedReason),
			Hold:        newHoldResponse(ended.Hold),
		})
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such hold on this task")
	case errors.Is(err, orchestrator.ErrHoldNotOwned):
		writeError(w, http.StatusForbidden, "the hold belongs to another session")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not renew hold")
	default:
		writeJSON(w, http.StatusOK, heartbeatResponse{Hold: newHoldResponse(hold)})
	}
}

// handleRelease serves POST /api/v1/tasks/{id}/release (LOOM-264). With
// hold_id it ends that hold as released, and answers 200 with the hold as
// it ended if it already had. Without one it is Take back, which any
// session may do: the task's drive hold, if any, ends as taken_back.
// Either way a task left in human_takeover goes back to awaiting_input.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if s.taskHolds == nil {
		writeError(w, http.StatusNotImplemented, "task takeover is not configured on this server")
		return
	}
	var req releaseRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	res, err := s.taskHolds.Release(r.Context(), id, req.HoldID)
	switch {
	case errors.Is(err, registry.ErrNotFound) && req.HoldID != "":
		writeError(w, http.StatusNotFound, "no such task, or no such hold on it")
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such task")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not release task")
	default:
		writeJSON(w, http.StatusOK, releaseResponse{TaskID: id, Status: apiTaskStatus(res.Task.Status), Hold: newHoldResponse(res.Hold)})
	}
}
