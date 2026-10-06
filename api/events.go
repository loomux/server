package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Loomux/server/registry"
)

// EventStore is what GET /conversations/{id}/events reads (LOOM-110):
// the conversation's dispatch audit trail.
type EventStore interface {
	ListDispatchEventsByConversation(ctx context.Context, conversationID string) ([]*registry.DispatchEvent, error)
}

// WithEvents enables GET /api/v1/conversations/{id}/events (LOOM-110).
func WithEvents(e EventStore) Option {
	return func(s *Server) { s.events = e }
}

// eventResponse is one audit-trail entry as clients see it. Empty fields
// are left out; duration is in milliseconds.
type eventResponse struct {
	ID          string    `json:"id"`
	DispatchID  string    `json:"dispatch_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Kind        string    `json:"kind"`
	Model       string    `json:"model,omitempty"`
	Tier        string    `json:"tier,omitempty"`
	TargetID    string    `json:"target_id,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	Command     string    `json:"command,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
	ErrorClass  string    `json:"error_class,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	Detail      string    `json:"detail,omitempty"`
}

// handleConversationEvents returns the conversation's audit trail, oldest
// first: what each turn decided (and which router model and tier), ran
// and where (commands redacted), and how it ended. An unknown
// conversation, or one past the retention period, has none.
func (s *Server) handleConversationEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeError(w, http.StatusNotImplemented, "the dispatch audit trail is not configured on this server")
		return
	}
	events, err := s.events.ListDispatchEventsByConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	out := make([]eventResponse, 0, len(events))
	for _, e := range events {
		out = append(out, eventResponse{
			ID: e.ID, DispatchID: e.DispatchID, CreatedAt: e.CreatedAt, Kind: e.Kind, Model: e.Model, Tier: e.Tier,
			TargetID: e.TargetID, WorkspaceID: e.WorkspaceID, TaskID: e.TaskID, Command: e.Command,
			Outcome: e.Outcome, ErrorClass: string(e.ErrorClass), DurationMS: e.Duration.Milliseconds(), Detail: e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": r.PathValue("id"), "events": out})
}
