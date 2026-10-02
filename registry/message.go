package registry

import "time"

// MessageRole identifies who produced a Message.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
)

// Message is one entry in a conversation's chat transcript — see design
// spec docs/design/message-logging-design.md. Append-only: nothing in
// this codebase ever updates or deletes a Message once written, so there
// is no UpdateMessage/DeleteMessage on Store.
type Message struct {
	ID string
	// ConversationID is a bare string, the same non-FK convention as
	// Task.ConversationID — a conversation isn't a stored entity.
	ConversationID string
	// TaskID is empty when this turn never touched a task (an
	// answer_directly turn).
	TaskID    string
	Role      MessageRole
	Content   string
	CreatedAt time.Time
}

// ConversationActivity is one conversation's footprint in the message
// log: every conversation with at least one logged turn has one, whether
// or not it ever touched a task (an answer_directly-only conversation
// has messages and no tasks — LOOM-62).
type ConversationActivity struct {
	ConversationID string
	// LastMessageAt is the CreatedAt of the conversation's most recent
	// message.
	LastMessageAt time.Time
}
