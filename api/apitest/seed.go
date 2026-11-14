package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// DefaultAgentType is the agent type of a seeded task that names none.
const DefaultAgentType = "claude-code"

// Login logs in over HTTP with the server's password and returns the
// session token, to send as "Authorization: Bearer <token>". Each call is
// a new session. A login refused by the backoff a wrong password just
// started (429; at most 10 ms on a test server) is retried.
func (s *Server) Login() string {
	s.tb.Helper()
	body, _ := json.Marshal(map[string]string{"password": s.Password})
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Post(s.URL+"/api/v1/login", "application/json", bytes.NewReader(body))
		if err != nil {
			s.tb.Fatalf("apitest: login: %v", err)
		}
		var out struct {
			Token string `json:"token"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil || resp.StatusCode != http.StatusOK || out.Token == "" {
			s.tb.Fatalf("apitest: login: status %d, %v", resp.StatusCode, err)
		}
		return out.Token
	}
}

// SeedTarget stores a remote target named name (host name + ".test",
// user "loomux"), so attach-info for a task on it carries an ssh attach
// command.
func (s *Server) SeedTarget(name string) *registry.Target {
	s.tb.Helper()
	target := &registry.Target{ID: s.nextID("target"), Name: name, Kind: registry.TargetKindRemote, Host: name + ".test", User: "loomux"}
	if err := s.Store.CreateTarget(context.Background(), target); err != nil {
		s.tb.Fatalf("apitest: seed target: %v", err)
	}
	return target
}

// SeedWorkspace stores a workspace named name on target; with a nil
// target, on a new one named name + "-target".
func (s *Server) SeedWorkspace(name string, target *registry.Target) *registry.Workspace {
	s.tb.Helper()
	if target == nil {
		target = s.SeedTarget(name + "-target")
	}
	ws := &registry.Workspace{ID: s.nextID("ws"), Name: name, Path: "/srv/" + name, TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := s.Store.CreateWorkspace(context.Background(), ws); err != nil {
		s.tb.Fatalf("apitest: seed workspace: %v", err)
	}
	return ws
}

// defaultWorkspace is the workspace of a seeded task that names none:
// "apitest", on a target of its own.
func (s *Server) defaultWorkspace() *registry.Workspace {
	s.tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace == nil {
		s.workspace = s.SeedWorkspace("apitest", nil)
	}
	return s.workspace
}

// Task describes a task to seed. Every field may be left zero.
type Task struct {
	// ID is the task's id; a fresh one when empty.
	ID string
	// ConversationID is the conversation the task belongs to; a fresh
	// one when empty. A conversation exists by having tasks or messages.
	ConversationID string
	// WorkspaceID is the task's workspace; the default workspace when
	// empty.
	WorkspaceID string
	// Status is the task's status; awaiting-input when empty. Statuses
	// are the store's spelling (registry.TaskStatusAwaitingInput); the
	// API shows them in snake_case.
	Status registry.TaskStatus
	// Kind is the task's kind; agent when empty. AgentType is its agent
	// type; DefaultAgentType when empty and the kind is agent.
	Kind      registry.TaskKind
	AgentType string
	// Attention is the prompt a needs-attention task is stopped at.
	Attention *registry.Attention
	// ErrorClass, FailureReason and OutputTail say why a failed task
	// failed.
	ErrorClass    registry.ErrorClass
	FailureReason string
	OutputTail    string
}

// SeedTask stores a task and gives it a session on the Executor, except
// a completed task, whose pane is taken to be gone. (A failed task gets
// one whatever its class, so every failed pane can be attached to.) The
// task is as a turn would have left it: nothing is running for it.
func (s *Server) SeedTask(seed Task) *registry.Task {
	s.tb.Helper()
	ctx := context.Background()
	if seed.ID == "" {
		seed.ID = s.nextID("task")
	}
	if seed.ConversationID == "" {
		seed.ConversationID = s.nextID("conv")
	}
	if seed.WorkspaceID == "" {
		seed.WorkspaceID = s.defaultWorkspace().ID
	}
	if seed.Status == "" {
		seed.Status = registry.TaskStatusAwaitingInput
	}
	if seed.Kind == "" {
		seed.Kind = registry.TaskKindAgent
	}
	if seed.Kind == registry.TaskKindAgent && seed.AgentType == "" {
		seed.AgentType = DefaultAgentType
	}
	now := time.Now().UTC()
	task := &registry.Task{
		ID:             seed.ID,
		WorkspaceID:    seed.WorkspaceID,
		Kind:           seed.Kind,
		AgentType:      seed.AgentType,
		TmuxSession:    "loomux-" + seed.ID,
		Status:         seed.Status,
		ConversationID: seed.ConversationID,
		StartedAt:      &now,
		Attention:      seed.Attention,
		ErrorClass:     seed.ErrorClass,
		FailureReason:  seed.FailureReason,
		OutputTail:     seed.OutputTail,
	}
	if seed.Status == registry.TaskStatusCompleted || seed.Status == registry.TaskStatusFailed {
		task.CompletedAt = &now
	}
	if err := s.Store.CreateTask(ctx, task); err != nil {
		s.tb.Fatalf("apitest: seed task: %v", err)
	}
	if seed.Status != registry.TaskStatusCompleted {
		if err := s.Executor.NewSession(ctx, task.TmuxSession, "", ""); err != nil {
			s.tb.Fatalf("apitest: seed task: %v", err)
		}
	}
	return task
}

// SeedPrompt stores a task stopped at a prompt only a person can answer
// (status needs-attention), in conversationID (a fresh conversation when
// empty). A zero prompt is a permission prompt for a Bash command with
// Yes and No.
func (s *Server) SeedPrompt(conversationID string, prompt registry.Attention) *registry.Task {
	s.tb.Helper()
	if prompt.Kind == "" {
		prompt = registry.Attention{
			Kind:     registry.AttentionPermission,
			Title:    "Bash command",
			Detail:   "rm -rf build/",
			Question: "Do you want to proceed?",
			Options:  []registry.AttentionOption{{Label: "Yes"}, {Label: "No"}},
		}
	}
	return s.SeedTask(Task{ConversationID: conversationID, Status: registry.TaskStatusNeedsAttention, Attention: &prompt})
}

// SeedFailedTask stores a failed task with class and reason, in
// conversationID (a fresh conversation when empty). With
// registry.ErrorClassLoginRequired, ErrorClassAgentRateLimited or
// ErrorClassAgentAPIError its pane is one kept for a person, which a
// takeover may hold.
func (s *Server) SeedFailedTask(conversationID string, class registry.ErrorClass, reason string) *registry.Task {
	s.tb.Helper()
	return s.SeedTask(Task{ConversationID: conversationID, Status: registry.TaskStatusFailed, ErrorClass: class, FailureReason: reason})
}

// SeedMessage appends a message to conversationID's transcript. task
// links it to a task and may be nil.
func (s *Server) SeedMessage(conversationID string, role registry.MessageRole, content string, task *registry.Task) *registry.Message {
	s.tb.Helper()
	msg := &registry.Message{ID: uuid.NewString(), ConversationID: conversationID, Role: role, Content: content}
	if task != nil {
		msg.TaskID = task.ID
	}
	if err := s.Store.CreateMessage(context.Background(), msg); err != nil {
		s.tb.Fatalf("apitest: seed message: %v", err)
	}
	return msg
}

// SeedConversation stores a conversation as one finished exchange leaves
// it: a user message, a task waiting for input, and the assistant's
// reply. It returns the task; its ConversationID is the conversation's
// (conversationID, or a fresh one when that is empty).
func (s *Server) SeedConversation(conversationID, userMessage, reply string) *registry.Task {
	s.tb.Helper()
	task := s.SeedTask(Task{ConversationID: conversationID})
	s.SeedMessage(task.ConversationID, registry.MessageRoleUser, userMessage, nil)
	s.SeedMessage(task.ConversationID, registry.MessageRoleAssistant, reply, task)
	return task
}

// SeedOffer stores an offer awaiting the user's yes (a pending
// confirmation) in offer.ConversationID. A zero Kind is run_command, a
// zero ExpiresAt ten minutes from now, and ID is filled when empty. The
// server knows only the offer's card: approving it in a dispatch reaches
// the Dispatcher's handler with ConfirmationID set, and what happens then
// is the handler's.
func (s *Server) SeedOffer(offer registry.Confirmation) *registry.Confirmation {
	s.tb.Helper()
	if offer.ID == "" {
		offer.ID = uuid.NewString()
	}
	if offer.ConversationID == "" {
		offer.ConversationID = s.nextID("conv")
	}
	if offer.Kind == "" {
		offer.Kind = registry.ConfirmationRunCommand
	}
	if offer.ExpiresAt.IsZero() {
		offer.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	}
	offer.Status = registry.ConfirmationPending
	if err := s.Store.CreateConfirmation(context.Background(), &offer); err != nil {
		s.tb.Fatalf("apitest: seed offer: %v", err)
	}
	return &offer
}

// SeedTurn stores one finished turn of task for GET
// /tasks/{id}/transcript: what was sent to the agent, its final message,
// and its pane.
func (s *Server) SeedTurn(task *registry.Task, userMessage, agentMessage, pane string) *registry.TaskTurn {
	s.tb.Helper()
	turn := &registry.TaskTurn{ID: uuid.NewString(), TaskID: task.ID, UserMessage: userMessage, AgentMessage: agentMessage, Pane: pane}
	if err := s.Store.CreateTaskTurn(context.Background(), turn); err != nil {
		s.tb.Fatalf("apitest: seed turn: %v", err)
	}
	return turn
}

// SetTaskStatus moves a task to status as the server itself would
// between turns, which an open conversation stream reports as a
// task_update. attention is the prompt for needs-attention and nil
// otherwise. It fails the test if the task's status changed since it was
// read, as a takeover by the client under test would make it.
func (s *Server) SetTaskStatus(taskID string, status registry.TaskStatus, attention *registry.Attention) *registry.Task {
	s.tb.Helper()
	ctx := context.Background()
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		s.tb.Fatalf("apitest: set task status: %v", err)
	}
	from := task.Status
	task.Status, task.Attention = status, attention
	if status == registry.TaskStatusCompleted || status == registry.TaskStatusFailed {
		now := time.Now().UTC()
		task.CompletedAt = &now
	}
	applied, err := s.Store.TransitionTask(ctx, task, from)
	if err != nil || !applied {
		s.tb.Fatalf("apitest: set task status: applied %v, %v", applied, err)
	}
	return task
}

// Task returns the stored task, failing the test if there is none.
func (s *Server) Task(id string) *registry.Task {
	s.tb.Helper()
	task, err := s.Store.GetTask(context.Background(), id)
	if err != nil {
		s.tb.Fatalf("apitest: task %q: %v", id, err)
	}
	return task
}

// ActiveHold returns the task's active drive hold, or nil if nobody
// holds it.
func (s *Server) ActiveHold(taskID string) *registry.TaskHold {
	s.tb.Helper()
	hold, err := s.Store.GetActiveDriveHold(context.Background(), taskID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.tb.Fatalf("apitest: active hold of %q: %v", taskID, err)
	}
	return hold
}
