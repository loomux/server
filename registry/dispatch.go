package registry

import (
	"errors"
	"time"
)

// DispatchStatus is where a dispatch job is in its life (LOOM-80, see
// docs/design/async-dispatch-design.md):
//
//	queued ──► running ──► succeeded
//	   │          ├──────► failed
//	   └──────────┴──────► interrupted
type DispatchStatus string

const (
	DispatchStatusQueued      DispatchStatus = "queued"
	DispatchStatusRunning     DispatchStatus = "running"
	DispatchStatusSucceeded   DispatchStatus = "succeeded"
	DispatchStatusFailed      DispatchStatus = "failed"
	DispatchStatusInterrupted DispatchStatus = "interrupted"
)

// Terminal reports whether s is an end state: nothing moves a job out
// of it.
func (s DispatchStatus) Terminal() bool {
	return s == DispatchStatusSucceeded || s == DispatchStatusFailed || s == DispatchStatusInterrupted
}

// ErrorClassInterrupted: the dispatch was cut off by the server shutting
// down or restarting, not by anything the turn itself did.
const ErrorClassInterrupted ErrorClass = "interrupted"

// Dispatch is one chat message's trip through the router, run as a job
// that outlives the HTTP request which submitted it (LOOM-80).
type Dispatch struct {
	ID             string
	ConversationID string
	// Message and WorkspaceHint are the request as submitted, kept so
	// the job can run from the row alone.
	Message       string
	WorkspaceHint string
	// IdempotencyKey is the client's Idempotency-Key, empty when none was
	// sent. Unique across dispatches when set.
	IdempotencyKey string
	// RequestHash fingerprints (conversation, message, hint), so a reused
	// key can be told apart from a retried request.
	RequestHash string
	Status      DispatchStatus
	// Reply is set once the job has succeeded; Error and ErrorClass once
	// it has failed or been interrupted.
	Reply      string
	Error      string
	ErrorClass ErrorClass
	CreatedAt  time.Time
	UpdatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// ErrIdempotencyKeyExists is returned by CreateDispatch when another
// dispatch already holds the key.
var ErrIdempotencyKeyExists = errors.New("registry: idempotency key already used")

// ErrConversationBusy is returned by CreateDispatch when the
// conversation already has a queued or running dispatch.
var ErrConversationBusy = errors.New("registry: conversation already has a dispatch in flight")

// ErrDispatchStateChanged is returned by TransitionDispatch when the job
// is no longer in the status the caller expected — someone else moved it
// first (e.g. shutdown marked it interrupted).
var ErrDispatchStateChanged = errors.New("registry: dispatch status changed")
