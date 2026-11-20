package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
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
	// EndSessionHolds ends a logged-out or revoked session's holds as
	// session_revoked, detaching their clients (LOOM-265). One it can't
	// end now is left for the hold sweeper.
	EndSessionHolds(ctx context.Context, sessionID string) (int, error)
	// ActiveHolds returns every hold that hasn't ended, and GetHold one
	// hold, active or ended (registry.ErrNotFound if unknown): what
	// conversations, attach-info and the stream show of holds
	// (LOOM-267).
	ActiveHolds(ctx context.Context) ([]*registry.TaskHold, error)
	GetHold(ctx context.Context, id string) (*registry.TaskHold, error)
}

// holdsOnTask is what a task shows of its holds (LOOM-267, LOOM-271):
// the one that drives it, if any, and those that watch it, oldest first.
// A watch hold is never shown as the drive hold: who is in the pane and
// who is only looking stay apart everywhere a client reads them.
type holdsOnTask struct {
	drive *registry.TaskHold
	watch []*registry.TaskHold
}

// activeHoldsByTask returns each task's active holds, by task id: what
// a hold shows on a task, its attach-info, its conversation's list row
// and the stream. A task with none isn't in it (its zero value shows
// nobody). Empty, not an error, on a server without holds.
func (s *Server) activeHoldsByTask(ctx context.Context) (map[string]holdsOnTask, error) {
	if s.taskHolds == nil {
		return map[string]holdsOnTask{}, nil
	}
	holds, err := s.taskHolds.ActiveHolds(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]holdsOnTask, len(holds))
	// Oldest acquired first, so each task's watch holds come in order.
	for _, h := range holds {
		of := out[h.TaskID]
		switch h.Mode {
		case registry.HoldModeDrive:
			of.drive = h
		case registry.HoldModeWatch:
			of.watch = append(of.watch, h)
		}
		out[h.TaskID] = of
	}
	return out, nil
}

// endSessionHoldsTimeout bounds how long a logout or revoke waits for
// its session's holds to end: detaching runs over SSH, and a target
// that doesn't answer leaves its hold to the sweeper rather than hold up
// the logout.
const endSessionHoldsTimeout = 15 * time.Second

// endSessionHolds ends sessionID's holds, once the session is deleted
// (logout, revoke), alongside its streams and waits. It doesn't fail
// the request: the token is already dead, and a hold left (its clients
// couldn't be detached) is ended by the sweeper, which finds its
// session gone. The orchestrator logs it.
func (s *Server) endSessionHolds(ctx context.Context, sessionID string) {
	if s.taskHolds == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endSessionHoldsTimeout)
	defer cancel()
	_, _ = s.taskHolds.EndSessionHolds(ctx, sessionID)
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
	// codeWatchUnsupported: takeover's 409 for mode watch on a machine
	// whose plugin has no read-only attach command (LOOM-272): no hold is
	// taken, and no writable command is handed out in its place.
	codeWatchUnsupported = "watch_unsupported"
	// codeHoldEnded: heartbeat's 410 for a hold that has ended (the
	// body's ended_reason says why).
	codeHoldEnded = "hold_ended"
	// codeDetachFailed: 502 from a Take back (release) or a forced
	// takeover whose holder's tmux clients couldn't be detached
	// (LOOM-265): nothing changes, the hold and the task's
	// human_takeover stay, so neither automation nor a second person
	// types into a pane its holder may still be in. Try again, or later.
	codeDetachFailed = "detach_failed"
)

// maxClientLabel bounds a hold's client_label, in characters.
const maxClientLabel = 64

// holdResponse is a registry.TaskHold as clients see it.
type holdResponse struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	// SessionID is the session holding the task, as GET /sessions names
	// it. ClientKind and ClientLabel describe that client ("tui",
	// "laptop"), copied from the session when the hold was taken
	// (client_label from the takeover's own, if it sent one); either may
	// be empty.
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

// newHoldResponses is holds as clients see them: an empty list, never
// null, when there are none.
func newHoldResponses(holds []*registry.TaskHold) []*holdResponse {
	out := make([]*holdResponse, 0, len(holds))
	for _, h := range holds {
		out = append(out, newHoldResponse(h))
	}
	return out
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
	// Force takes the task from another session that holds it. It is for
	// a drive takeover: with watch it is refused.
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
// caller's session takes a hold on the task, and gets the attach-info
// with it.
//
// A drive hold (the default) pauses automation into the task's pane. The
// hold is checked before the task's status: the session's own hold is
// renewed and returned, another session's is a 409 task_held unless
// force.
//
// A watch hold (mode: "watch", LOOM-271) only says who is looking: the
// task, its driver and its turn are left as they are, and it is never
// refused as held. Its attach carries read-only commands only
// (LOOM-272): a session that asked only to look is never handed a way
// to type. A machine whose plugin has no read-only command is 409
// watch_unsupported, and no hold is taken.
//
// See orchestrator.Takeover.
func (s *Server) handleTakeover(w http.ResponseWriter, r *http.Request) {
	if s.taskHolds == nil {
		writeError(w, http.StatusNotImplemented, "task takeover is not configured on this server")
		return
	}
	var req takeoverRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	mode := registry.HoldMode(req.Mode)
	switch mode {
	case "":
		mode = registry.HoldModeDrive
	case registry.HoldModeDrive, registry.HoldModeWatch:
	default:
		writeError(w, http.StatusBadRequest, `mode must be "drive" or "watch"`)
		return
	}
	watch := mode == registry.HoldModeWatch
	if watch && req.Force {
		// Refused, not ignored: force detaches a pane's holder, which a
		// request to look must never be read as asking for.
		writeError(w, http.StatusBadRequest, "force takes a task from the session driving it; it can't be used with watch")
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
	// takeover never succeeds without its attach-info. A watch's is read
	// without the writable commands, so none is ever in hand to send,
	// and given the read-only ones; a machine that has none is refused
	// here, unheld.
	attach, ok := s.attachInfoFor(w, r, id, !watch)
	if !ok {
		return
	}
	if watch && !s.setReadOnlyAttach(w, r, &attach) {
		return
	}
	// Who holds it is who the session is (LOOM-268): its client kind,
	// and its label unless this takeover names another.
	sess := sessionFromContext(r.Context())
	if label == "" {
		label = sess.Label
	}
	res, err := s.taskHolds.Takeover(r.Context(), orchestrator.TakeoverRequest{
		TaskID:      id,
		SessionID:   sess.ID,
		ClientKind:  sess.ClientKind,
		ClientLabel: label,
		Mode:        mode,
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
	case errors.Is(err, orchestrator.ErrDetachFailed):
		writeErrorCode(w, http.StatusBadGateway, codeDetachFailed,
			"the terminal attached to this task couldn't be detached, so it wasn't taken; try again")
	case errors.Is(err, orchestrator.ErrTaskInactive):
		writeErrorCode(w, http.StatusConflict, codeTaskInactive, "the task has ended and its pane wasn't kept for a person")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such task")
	case errors.Is(err, orchestrator.ErrStatusChanged):
		writeError(w, http.StatusConflict, "the task kept changing while it was being taken over; look at it again")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not take over task")
	case !s.sessionStillValid(r.Context(), sess):
		// A logout or revoke that ran while this was being taken ended
		// the session's holds without this one: it is ended here,
		// rather than the pane handed to a dead session (LOOM-265), as a
		// dispatch's wait checks its session again (LOOM-182).
		s.endSessionHolds(r.Context(), sess.ID)
		writeError(w, http.StatusUnauthorized, "invalid or expired session")
	case res.Hold.Mode != mode:
		// Never reached: Takeover gives the mode asked for. Checked all
		// the same, because the other way round is a session that asked
		// to look being told it holds the pane, or one that took the
		// pane being told it only looks. The hold nobody is told of is
		// given back (nobody attached on it, so nobody is detached).
		_, _ = s.taskHolds.Release(context.WithoutCancel(r.Context()), id, res.Hold.ID)
		writeError(w, http.StatusInternalServerError, "could not take over task")
	default:
		// The attach-info was read before the hold was taken: it carries
		// the hold just taken, as a read of it now would.
		hold := newHoldResponse(res.Hold)
		if watch {
			attach.WatchHolds = withHold(attach.WatchHolds, hold)
		} else {
			attach.Hold = hold
		}
		writeJSON(w, http.StatusOK, takeoverResponse{TaskID: id, Hold: hold, Attach: attach})
	}
}

// setReadOnlyAttach gives a watch's attach-info, read without commands,
// the commands to watch with (LOOM-272): attach_command and
// attach_commands that attach read-only (tmux's -r), so whoever runs one
// can't type into the pane. The local and ssh forms are built here; a
// plugin's own are asked of it read-only and checked
// (readOnlyAttachCommandsFor). On failure it writes the response and
// reports false: 409 watch_unsupported when the target is a machine
// whose plugin has no read-only command.
func (s *Server) setReadOnlyAttach(w http.ResponseWriter, r *http.Request, attach *attachInfoResponse) bool {
	target, err := s.attachInfo.GetTarget(r.Context(), attach.Target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve workspace's target")
		return false
	}
	commands, unsupported := s.readOnlyAttachCommandsFor(r.Context(), target, attach.TmuxSession)
	if unsupported != nil {
		writeErrorCode(w, http.StatusConflict, codeWatchUnsupported, fmt.Sprintf(
			"the %s plugin (%s) has no read-only attach command, so this machine can't be watched; upgrade the plugin, or attach to drive",
			unsupported.Plugin, unsupported.Version))
		return false
	}
	attach.AttachCommand = targets.ReadOnlyAttachCommand(s.tmuxSocketName(), attach.TmuxSession)
	attach.AttachCommands = commands
	return true
}

// withHold is holds with hold in it: in place of the entry with its id
// (a renewal), or else at the end, as the newest.
func withHold(holds []*holdResponse, hold *holdResponse) []*holdResponse {
	for i, h := range holds {
		if h.ID == hold.ID {
			holds[i] = hold
			return holds
		}
	}
	return append(holds, hold)
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
// session may do: the task's drive hold, if any, ends as taken_back once
// its tmux clients are detached (502 detach_failed if they can't be,
// LOOM-265). Either way a task left in human_takeover goes back to
// awaiting_input.
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
	case errors.Is(err, orchestrator.ErrDetachFailed):
		writeErrorCode(w, http.StatusBadGateway, codeDetachFailed,
			"the terminal attached to this task couldn't be detached, so it is still held; try again")
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
