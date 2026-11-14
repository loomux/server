package registry

import (
	"errors"
	"time"
)

// HoldMode is what a hold lets its holder do (LOOM-263,
// docs/design/tui-client-design.md "Server awareness: holds").
type HoldMode string

const (
	// HoldModeDrive: the holder types into the task's pane. At most one
	// active drive hold exists per task.
	HoldModeDrive HoldMode = "drive"
	// HoldModeWatch: the holder is attached read-only. Any number may be
	// active per task, beside at most one drive hold.
	HoldModeWatch HoldMode = "watch"
)

// Valid reports whether m is a known mode.
func (m HoldMode) Valid() bool { return m == HoldModeDrive || m == HoldModeWatch }

// HoldEndReason is why a hold ended.
type HoldEndReason string

const (
	// HoldEndReleased: its holder gave it back.
	HoldEndReleased HoldEndReason = "released"
	// HoldEndExpired: its lease ran out without a heartbeat.
	HoldEndExpired HoldEndReason = "expired"
	// HoldEndTakenBack: another client ended it without taking the task
	// itself (Take back).
	HoldEndTakenBack HoldEndReason = "taken_back"
	// HoldEndTakenOver: another session took the task with force.
	HoldEndTakenOver HoldEndReason = "taken_over"
	// HoldEndSessionRevoked: its session was logged out, revoked or
	// expired.
	HoldEndSessionRevoked HoldEndReason = "session_revoked"
	// HoldEndTaskEnded: the held task ended.
	HoldEndTaskEnded HoldEndReason = "task_ended"
)

// Valid reports whether r is a known reason.
func (r HoldEndReason) Valid() bool {
	switch r {
	case HoldEndReleased, HoldEndExpired, HoldEndTakenBack, HoldEndTakenOver, HoldEndSessionRevoked, HoldEndTaskEnded:
		return true
	}
	return false
}

// ErrTaskHeld is returned by CreateTaskHold and AcquireDriveHold when
// the task already has an active drive hold: there is one per task.
var ErrTaskHeld = errors.New("registry: task already has an active drive hold")

// ErrHoldEnded is returned by RenewTaskHold for a hold that has ended;
// the hold comes back with it, saying why.
var ErrHoldEnded = errors.New("registry: hold has ended")

// TaskHold is a lease on one task by one API session (LOOM-263): who is
// attached to the task's pane, since when, and until when unless
// renewed. The hold says who; the task's status (human-takeover) still
// says whether automation may type. A hold is active until EndedAt is
// set, and an ended one is kept as the audit trail until retention
// deletes it.
type TaskHold struct {
	ID     string
	TaskID string
	// SessionID is the API session that took the hold. It is not a
	// foreign key: the session may be deleted while the ended hold is
	// kept.
	SessionID string
	// ClientKind and ClientLabel describe the holder for other clients
	// ("tui", "laptop"). Either may be empty.
	ClientKind  string
	ClientLabel string
	Mode        HoldMode
	// PrevStatus is the task's status when the hold was taken, for
	// display and audit.
	PrevStatus TaskStatus
	// TTL is the lease's length: the hold expires TTL after it was
	// acquired or last renewed.
	TTL        time.Duration
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
	// EndedAt and EndedReason are set once, when the hold ends.
	EndedAt     *time.Time
	EndedReason HoldEndReason
}

// HoldRelease is what ReleaseTaskHold did.
type HoldRelease struct {
	// Hold is the hold named, or the task's active drive hold when none
	// was named, as it stands now; nil when none was named and the task
	// had none.
	Hold *TaskHold
	// Ended reports whether this call ended Hold; false for one that had
	// already ended, which keeps its own time and reason.
	Ended bool
	// Task is the task as it stands now, and TaskReleased whether this
	// call moved it from human-takeover to awaiting-input.
	Task         *Task
	TaskReleased bool
}

// Active reports whether the hold has not ended. A hold past ExpiresAt
// is still active until something ends it as expired.
func (h *TaskHold) Active() bool { return h.EndedAt == nil }
