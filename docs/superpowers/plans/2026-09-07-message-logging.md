# Message/Turn-Level Logging (LOOM-31) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist a chat transcript (user/assistant message pairs, one pair per `Router.Dispatch` turn) and expose it via the existing `GET /conversations/{id}` endpoint, closing the gap `docs/design/web-client-design.md` flags as "Known API gap: no persisted message transcript."

**Architecture:** A new `messages` table + `registry.Message` domain type, added to the existing `registry.Store` interface and its sqlite implementation (same swappable-backend pattern as `Target`/`Workspace`/`Task`). `Router.Dispatch` writes one `[user, assistant]` message pair per turn, only once a reply is actually produced (never on an error path). `GET /conversations/{id}` gains an additive `messages` field and its 404 condition widens to "no tasks AND no messages."

**Tech Stack:** Go, `database/sql` + `modernc.org/sqlite`, `goose` migrations, standard library `net/http`/`net/http/httptest` for API tests.

**Spec:** `docs/design/message-logging-design.md` — read this in full before starting; every task below implements a specific section of it.

## Global Constraints

- No new dependencies — everything needed already exists in `go.mod`.
- Every store-layer change must go through `registry/storetest`'s shared conformance suite (design spec's Testing Strategy: "adding a new backend means passing the existing suite").
- `Message` is append-only: no `UpdateMessage`, no `DeleteMessage`, ever.
- Messages are written **only once a reply is actually available** — never on an error path (design doc, "Where it's written").
- `messages.task_id` is `ON DELETE SET NULL`, not `RESTRICT` like every other FK in this schema.
- Run `gofmt -l .` (expect no output) and `go vet ./...` before each commit in addition to `go test`.
- Follow this repo's existing narrow-interface style exactly (`TaskLister`, `WorkspaceLister`, etc. in `api/server.go`) — don't introduce a different pattern.

---

### Task 1: Message storage — domain type, migration, sqlite implementation, conformance suite

**Files:**
- Create: `registry/message.go`
- Modify: `registry/store.go` (add `CreateMessage`/`ListMessagesByConversation` to the `Store` interface)
- Create: `registry/sqlite/migrations/00007_create_messages.sql`
- Modify: `registry/sqlite/sqlite.go` (implement the two new methods + `scanMessage`)
- Modify: `registry/storetest/storetest.go` (conformance test cases + wire into `Run`)

**Interfaces:**
- Produces: `registry.Message` struct, `registry.MessageRole` (`registry.MessageRoleUser`, `registry.MessageRoleAssistant`), and two new `registry.Store` methods:
  - `CreateMessage(ctx context.Context, m *Message) error`
  - `ListMessagesByConversation(ctx context.Context, conversationID string) ([]*Message, error)` — oldest first, empty slice (not an error) for an unknown `conversationID`.

- [ ] **Step 1: Write the conformance test cases in `registry/storetest/storetest.go`**

Add these functions anywhere after `testDeleteTargetWithWorkspacesRejected` (end of the Task section) and before the Credential section's `testCredentialCRUD`:

```go
func testMessageCRUD(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID: "msg-task-" + t.Name(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent,
		AgentType: "claude-code", TmuxSession: "sess-" + t.Name(), Status: registry.TaskStatusRunning,
		ConversationID: "conv-1",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	msg := &registry.Message{
		ID:             "msg-1-" + t.Name(),
		ConversationID: "conv-1",
		TaskID:         task.ID,
		Role:           registry.MessageRoleUser,
		Content:        "hello there",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if msg.CreatedAt.IsZero() {
		t.Fatalf("CreateMessage did not set CreatedAt: %+v", msg)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListMessagesByConversation = %+v, want 1 message", list)
	}
	got := list[0]
	if got.ID != msg.ID || got.ConversationID != "conv-1" || got.TaskID != task.ID ||
		got.Role != registry.MessageRoleUser || got.Content != "hello there" {
		t.Fatalf("ListMessagesByConversation[0] = %+v, want %+v", got, msg)
	}
}

func testMessageWithoutTask(t *testing.T, s registry.Store) {
	ctx := context.Background()
	msg := &registry.Message{
		ID:             "msg-direct-" + t.Name(),
		ConversationID: "conv-direct",
		Role:           registry.MessageRoleAssistant,
		Content:        "a direct answer",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage (no task): %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-direct")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 || list[0].TaskID != "" {
		t.Fatalf("ListMessagesByConversation = %+v, want 1 message with empty TaskID", list)
	}
}

// testMessageListByConversationOrdering deliberately inserts three
// messages back-to-back with no artificial delay between them, to prove
// ordering is guaranteed by insertion order and doesn't depend on
// created_at having enough timestamp resolution to distinguish rows
// written in the same instant.
func testMessageListByConversationOrdering(t *testing.T, s registry.Store) {
	ctx := context.Background()
	mk := func(id, conversationID, content string) *registry.Message {
		return &registry.Message{ID: id, ConversationID: conversationID, Role: registry.MessageRoleUser, Content: content}
	}
	if err := s.CreateMessage(ctx, mk("m1", "conv-order", "first")); err != nil {
		t.Fatalf("CreateMessage(m1): %v", err)
	}
	if err := s.CreateMessage(ctx, mk("m2", "conv-order", "second")); err != nil {
		t.Fatalf("CreateMessage(m2): %v", err)
	}
	if err := s.CreateMessage(ctx, mk("m3", "conv-order", "third")); err != nil {
		t.Fatalf("CreateMessage(m3): %v", err)
	}
	// An unrelated conversation must not leak in.
	if err := s.CreateMessage(ctx, mk("m-other", "conv-other", "unrelated")); err != nil {
		t.Fatalf("CreateMessage(m-other): %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-order")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListMessagesByConversation = %+v, want 3 messages", list)
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if list[i].Content != w {
			t.Fatalf("ListMessagesByConversation[%d].Content = %q, want %q (insertion order = %+v)", i, list[i].Content, w, list)
		}
	}
}

func testMessageListByConversationUnknownReturnsEmpty(t *testing.T, s registry.Store) {
	list, err := s.ListMessagesByConversation(context.Background(), "no-such-conversation")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListMessagesByConversation(unknown) = %+v, want empty", list)
	}
}

func testMessageRequiresValidTaskWhenSet(t *testing.T, s registry.Store) {
	ctx := context.Background()
	msg := &registry.Message{
		ID: "msg-bad-task", ConversationID: "conv-1", TaskID: "does-not-exist",
		Role: registry.MessageRoleUser, Content: "hi",
	}
	if err := s.CreateMessage(ctx, msg); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("CreateMessage with an unknown task id: err = %v, want ErrConflict", err)
	}
}

func testMessageSurvivesTaskDeletion(t *testing.T, s registry.Store) {
	ctx := context.Background()
	ws := createTestWorkspace(t, s)
	task := &registry.Task{
		ID: "msg-del-task", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent,
		AgentType: "claude-code", TmuxSession: "sess-del", Status: registry.TaskStatusCompleted,
		ConversationID: "conv-del",
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	msg := &registry.Message{
		ID: "msg-del-1", ConversationID: "conv-del", TaskID: task.ID,
		Role: registry.MessageRoleUser, Content: "hi",
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	list, err := s.ListMessagesByConversation(ctx, "conv-del")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListMessagesByConversation = %+v, want the message to survive task deletion", list)
	}
	if list[0].TaskID != "" {
		t.Fatalf("message TaskID = %q after its task was deleted, want empty (ON DELETE SET NULL)", list[0].TaskID)
	}
}
```

Then register them in `Run` (add after the existing `t.Run("DeleteTargetWithWorkspacesRejected", ...)` line, before the Credential block's `t.Run("Credential", ...)`):

```go
	t.Run("Message", func(t *testing.T) { testMessageCRUD(t, newStore(t)) })
	t.Run("MessageWithoutTask", func(t *testing.T) { testMessageWithoutTask(t, newStore(t)) })
	t.Run("MessageListByConversationOrdering", func(t *testing.T) { testMessageListByConversationOrdering(t, newStore(t)) })
	t.Run("MessageListByConversationUnknownReturnsEmpty", func(t *testing.T) { testMessageListByConversationUnknownReturnsEmpty(t, newStore(t)) })
	t.Run("MessageRequiresValidTaskWhenSet", func(t *testing.T) { testMessageRequiresValidTaskWhenSet(t, newStore(t)) })
	t.Run("MessageSurvivesTaskDeletion", func(t *testing.T) { testMessageSurvivesTaskDeletion(t, newStore(t)) })
```

- [ ] **Step 2: Run the suite and confirm it fails to compile**

Run: `go test ./registry/... 2>&1 | head -30`
Expected: build failure — `registry.Message`, `registry.MessageRole`, `registry.MessageRoleUser`, `registry.MessageRoleAssistant`, `s.CreateMessage`, `s.ListMessagesByConversation` all undefined. This is the "failing test" for this task — the type/interface don't exist yet.

- [ ] **Step 3: Add the domain type — create `registry/message.go`**

```go
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
```

- [ ] **Step 4: Add the two methods to the `Store` interface in `registry/store.go`**

Insert immediately after the existing `DeleteTask(ctx context.Context, id string) error` line, before the `// CreateCredential, GetCredential, ...` comment block:

```go
	// CreateMessage and ListMessagesByConversation store the per-turn
	// chat transcript (design spec docs/design/message-logging-design.md).
	// Messages are append-only — there is no update/delete here by
	// design (see registry.Message's doc comment).
	CreateMessage(ctx context.Context, m *Message) error
	// ListMessagesByConversation returns every message for
	// conversationID, oldest first (insertion order). An unknown
	// conversationID returns an empty slice, not an error — mirrors
	// ListTasks's own "conversation isn't a stored entity" stance;
	// there's nothing to 404 on at this layer.
	ListMessagesByConversation(ctx context.Context, conversationID string) ([]*Message, error)
```

- [ ] **Step 5: Add the migration — create `registry/sqlite/migrations/00007_create_messages.sql`**

```sql
-- +goose Up
CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    task_id         TEXT REFERENCES tasks (id) ON DELETE SET NULL,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

CREATE INDEX idx_messages_conversation_id ON messages (conversation_id);
CREATE INDEX idx_messages_task_id ON messages (task_id);

-- +goose Down
DROP TABLE messages;
```

- [ ] **Step 6: Implement the sqlite methods in `registry/sqlite/sqlite.go`**

Insert after `ListTasks` and its `scanTask` helper (i.e. right before `func (s *Store) CreateCredential`):

```go
func (s *Store) CreateMessage(ctx context.Context, m *registry.Message) error {
	m.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, task_id, role, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, nullIfEmpty(m.TaskID), string(m.Role), m.Content, m.CreatedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: task %q does not exist", registry.ErrConflict, m.TaskID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create message: %w", err)
	}
	return nil
}

const messageColumns = `id, conversation_id, task_id, role, content, created_at`

// ListMessagesByConversation orders by created_at then the table's
// implicit rowid — the rowid tiebreak guarantees insertion order even
// when two messages in the same turn (a user message immediately
// followed by its assistant reply) land on a created_at value with
// insufficient resolution to distinguish them on its own.
func (s *Store) ListMessagesByConversation(ctx context.Context, conversationID string) ([]*registry.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM messages
		WHERE conversation_id = ?
		ORDER BY created_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
	}
	defer rows.Close()

	out := make([]*registry.Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
	}
	return out, nil
}

func scanMessage(row rowScanner) (*registry.Message, error) {
	var m registry.Message
	var taskID sql.NullString
	var role string
	if err := row.Scan(&m.ID, &m.ConversationID, &taskID, &role, &m.Content, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.TaskID = taskID.String
	m.Role = registry.MessageRole(role)
	return &m, nil
}
```

- [ ] **Step 7: Run the suite and confirm it passes**

Run: `go test ./registry/... -v -run Message 2>&1 | tail -60`
Expected: `PASS` for every `TestStore/Message*` subtest.

Then run the full registry suite to make sure nothing else broke: `go test ./registry/...`
Expected: `ok`.

- [ ] **Step 8: Format, vet, commit**

```bash
gofmt -l registry/
go vet ./registry/...
git add registry/message.go registry/store.go registry/sqlite/sqlite.go \
  registry/sqlite/migrations/00007_create_messages.sql registry/storetest/storetest.go
git commit -m "$(cat <<'EOF'
feat(registry): add message/turn-level chat transcript storage (LOOM-31)

New Message domain type + Store.CreateMessage/ListMessagesByConversation,
following the existing swappable-backend pattern. Append-only by design;
task_id is ON DELETE SET NULL so a message survives its task's deletion.
EOF
)"
```

---

### Task 2: Router wiring — persist a message pair per turn

**Files:**
- Modify: `router/router.go` (add `logTurn` helper, call it from `Dispatch`'s `answer_directly` branch and from the end of `dispatchToAgent`)
- Create: `router/message_logging_test.go`

**Interfaces:**
- Consumes: `registry.Message`, `registry.MessageRoleUser`, `registry.MessageRoleAssistant`, `Store.CreateMessage`, `Store.ListMessagesByConversation` (Task 1); `uuid.NewString()` (already imported in `router.go`).
- Produces: no new exported symbols — this task only changes `Router`'s internal behavior. Task 3 depends on messages existing in the store after a `Dispatch` call, not on anything new exported here.

- [ ] **Step 1: Write the failing tests — create `router/message_logging_test.go`**

```go
package router_test

import (
	"context"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func TestDispatch_AnswerDirectly_LogsMessagePair(t *testing.T) {
	store, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "what's up"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != registry.MessageRoleUser || msgs[0].Content != "what's up" {
		t.Fatalf("msgs[0] = %+v, want role=user content=%q", msgs[0], "what's up")
	}
	if msgs[1].Role != registry.MessageRoleAssistant || msgs[1].Content != "the answer" {
		t.Fatalf("msgs[1] = %+v, want role=assistant content=%q", msgs[1], "the answer")
	}
	if msgs[0].TaskID != "" || msgs[1].TaskID != "" {
		t.Fatalf("msgs = %+v, want empty TaskID for an answer_directly turn", msgs)
	}
}

func TestDispatch_UseWorkspace_LogsMessagePairWithTaskID(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "condensed reply", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "do the thing"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace: tasks=%+v err=%v", tasks, err)
	}
	taskID := tasks[0].ID

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].TaskID != taskID || msgs[1].TaskID != taskID {
		t.Fatalf("msgs = %+v, want TaskID = %q", msgs, taskID)
	}
	if msgs[0].Content != "do the thing" || msgs[1].Content != "condensed reply" {
		t.Fatalf("msgs content = [%q, %q], want [%q, %q]", msgs[0].Content, msgs[1].Content, "do the thing", "condensed reply")
	}
}

func TestDispatch_SecondTurnSameTask_AppendsMoreMessages(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	reply := "first reply"
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: reply, Done: false}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "first message"); err != nil {
		t.Fatalf("Dispatch (first turn): %v", err)
	}
	reply = "second reply"
	if _, err := r.Dispatch(context.Background(), "conv-1", "second message"); err != nil {
		t.Fatalf("Dispatch (second turn): %v", err)
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (two turns)", len(msgs))
	}
	wantContent := []string{"first message", "first reply", "second message", "second reply"}
	for i, want := range wantContent {
		if msgs[i].Content != want {
			t.Fatalf("msgs[%d].Content = %q, want %q (full order = %+v)", i, msgs[i].Content, want, msgs)
		}
	}
}

func TestDispatch_Failure_LogsNothing(t *testing.T) {
	store, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: "does-not-exist", AgentType: "claude-code"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err == nil {
		t.Fatal("Dispatch against an unknown workspace: got nil error")
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("ListMessagesByConversation = %+v, want no messages logged for a failed dispatch", msgs)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./router/... -run 'TestDispatch_AnswerDirectly_LogsMessagePair|TestDispatch_UseWorkspace_LogsMessagePairWithTaskID|TestDispatch_SecondTurnSameTask_AppendsMoreMessages|TestDispatch_Failure_LogsNothing' -v`
Expected: the first three FAIL (`got 0 messages, want 2`/`4`); `TestDispatch_Failure_LogsNothing` passes trivially already (nothing is logged today) — that's fine, it becomes a real regression guard once Step 3 lands.

- [ ] **Step 3: Implement the logging hook in `router/router.go`**

Add this method anywhere in the file (e.g. directly after `Dispatch`):

```go
// logTurn persists one turn's user/assistant message pair. Called only
// once a reply is actually available (design spec
// docs/design/message-logging-design.md, "Where it's written") — a
// Dispatch call that errors before producing a reply leaves no trace
// here; the caller already learns about the failure synchronously via
// Dispatch's own returned error.
func (r *Router) logTurn(ctx context.Context, conversationID, taskID, userMessage, assistantReply string) error {
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		TaskID:         taskID,
		Role:           registry.MessageRoleUser,
		Content:        userMessage,
	}); err != nil {
		return fmt.Errorf("log turn: user message: %w", err)
	}
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		TaskID:         taskID,
		Role:           registry.MessageRoleAssistant,
		Content:        assistantReply,
	}); err != nil {
		return fmt.Errorf("log turn: assistant message: %w", err)
	}
	return nil
}
```

Then change the `ActionAnswerDirectly` case inside `Dispatch` from:

```go
	case ActionAnswerDirectly:
		return decision.DirectAnswer, nil
```

to:

```go
	case ActionAnswerDirectly:
		if err := r.logTurn(ctx, conversationID, "", message, decision.DirectAnswer); err != nil {
			return "", fmt.Errorf("router: dispatch: %w", err)
		}
		return decision.DirectAnswer, nil
```

Then change the tail of `dispatchToAgent` from:

```go
	if result.Done {
		if err := r.orch.Complete(ctx, task.ID, result.Reply); err != nil {
			return "", fmt.Errorf("router: dispatch: complete: %w", err)
		}
	} else if err := r.store.SetWorkspaceRollingSummary(ctx, workspaceID, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: update rolling summary: %w", err)
	}

	return result.Reply, nil
}
```

to:

```go
	if result.Done {
		if err := r.orch.Complete(ctx, task.ID, result.Reply); err != nil {
			return "", fmt.Errorf("router: dispatch: complete: %w", err)
		}
	} else if err := r.store.SetWorkspaceRollingSummary(ctx, workspaceID, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: update rolling summary: %w", err)
	}

	if err := r.logTurn(ctx, conversationID, task.ID, message, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	return result.Reply, nil
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./router/... -v -run TestDispatch 2>&1 | tail -80`
Expected: every `TestDispatch_*` test PASSes, including all four new ones and every pre-existing one in `dispatch_test.go`/`continuation_test.go` (this proves the new writes didn't break any existing behavior).

Then run the full router suite: `go test ./router/...`
Expected: `ok`.

- [ ] **Step 5: Format, vet, commit**

```bash
gofmt -l router/
go vet ./router/...
git add router/router.go router/message_logging_test.go
git commit -m "$(cat <<'EOF'
feat(router): log a user/assistant message pair per Dispatch turn (LOOM-31)

Writes both rows only once a reply is actually produced — never on an
error path — for both the answer_directly branch and agent dispatch.
EOF
)"
```

---

### Task 3: API — expose messages via `GET /conversations/{id}`

**Files:**
- Modify: `api/server.go` (new `MessageLister` interface, `Server.messages` field, `NewServer` gains a parameter, `getConversationResponse`/`messageSummary`, `handleGetConversation`)
- Modify: `api/server_test.go` (`newTestServer` helper's `NewServer` call, new fixture helper, new tests)
- Modify: `api/integration_test.go` (`NewServer` call, new end-to-end test)
- Modify: `cmd/loomuxd/main.go` (`NewServer` call)
- Modify: `api/README.md` (update the `GET /api/v1/conversations/{id}` bullet)

**Interfaces:**
- Consumes: `registry.Message`, `Store.ListMessagesByConversation` (Task 1); the fact that a real `Dispatch` call now writes messages (Task 2), used by the integration test.
- Produces: `messageSummary` JSON shape `{id, role, content, task_id?, created_at}`; `getConversationResponse.Messages []messageSummary`.

- [ ] **Step 1: Write the failing tests**

In `api/server_test.go`, add this fixture helper near `createTestTask`:

```go
// createTestMessage inserts a message directly via the store, as a
// fixture for conversation-endpoint tests that aren't exercising
// message-creation behavior themselves.
func createTestMessage(t *testing.T, s registry.Store, id, conversationID, taskID string, role registry.MessageRole, content string) *registry.Message {
	t.Helper()
	msg := &registry.Message{ID: id, ConversationID: conversationID, TaskID: taskID, Role: role, Content: content}
	if err := s.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	return msg
}
```

Add these test functions near the existing `TestGetConversation_*` tests:

```go
func TestGetConversation_ValidToken_IncludesMessagesOldestFirst(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws-a", registry.WorkspaceStatusIdle)
	task := createTestTask(t, store, "task-1", ws.ID, "conv-msgs", registry.TaskStatusCompleted)
	createTestMessage(t, store, "m1", "conv-msgs", task.ID, registry.MessageRoleUser, "hi there")
	createTestMessage(t, store, "m2", "conv-msgs", task.ID, registry.MessageRoleAssistant, "hello!")
	// An unrelated conversation must not leak in.
	createTestMessage(t, store, "m-other", "conv-other", "", registry.MessageRoleUser, "unrelated")

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-msgs", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		Messages []struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
			TaskID  string `json:"task_id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].ID != "m1" || out.Messages[0].Role != "user" || out.Messages[0].Content != "hi there" || out.Messages[0].TaskID != task.ID {
		t.Fatalf("messages[0] = %+v, want the user message first", out.Messages[0])
	}
	if out.Messages[1].ID != "m2" || out.Messages[1].Role != "assistant" || out.Messages[1].Content != "hello!" {
		t.Fatalf("messages[1] = %+v, want the assistant message second", out.Messages[1])
	}
}

func TestGetConversation_MessagesOnlyNoTasks_ReturnsOK(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestMessage(t, store, "m1", "conv-direct-only", "", registry.MessageRoleUser, "what's up")
	createTestMessage(t, store, "m2", "conv-direct-only", "", registry.MessageRoleAssistant, "not much")

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-direct-only", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (a message-only, task-less conversation must not 404)", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		Tasks    []any `json:"tasks"`
		Messages []any `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("got %d tasks, want 0", len(out.Tasks))
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(out.Messages))
	}
}
```

Update `newTestServer` in `api/server_test.go` — change:

```go
	server := api.NewServer(dispatcher, store, store, store, store, []byte(hash), opts...)
```

to:

```go
	server := api.NewServer(dispatcher, store, store, store, store, store, []byte(hash), opts...)
```

(`store` satisfies `MessageLister` structurally, same as it already does for `SessionStore`/`WorkspaceLister`/`TaskLister`/`AttachInfoStore`.)

In `api/integration_test.go`, update the existing call:

```go
	server := api.NewServer(realApp, realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
```

to:

```go
	server := api.NewServer(realApp, realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
```

Then add a new end-to-end test in `api/integration_test.go` (reuses `routerConfigFor`, `doLogin`, `postJSON` already in that file):

```go
// TestIntegration_DispatchThenConversationDetail_ShowsMessages proves
// LOOM-31 end-to-end: a real POST /dispatch call against the real stack
// (real sqlite, a fake LLM vendor) results in a subsequent
// GET /conversations/{id} response that includes the turn's actual
// message text, not just task-lifecycle rows.
func TestIntegration_DispatchThenConversationDetail_ShowsMessages(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{
							"name":      "route_decision",
							"arguments": `{"action":"answer_directly","direct_answer":"the transcript works"}`,
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(llmSrv.Close)

	realApp, err := app.Build(app.Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Router: routerConfigFor(llmSrv.URL),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	t.Cleanup(func() {
		if err := realApp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	hash, err := api.HashPassword("integration-test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	server := api.NewServer(realApp, realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)

	token, status := doLogin(t, httpSrv.URL, "integration-test-password")
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}

	dispatchResp := postJSON(t, httpSrv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-transcript", "message": "hello"})
	dispatchResp.Body.Close()
	if dispatchResp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch status = %d, want %d", dispatchResp.StatusCode, http.StatusOK)
	}

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/conversations/c-transcript", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	convResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer convResp.Body.Close()
	if convResp.StatusCode != http.StatusOK {
		t.Fatalf("conversation detail status = %d, want %d", convResp.StatusCode, http.StatusOK)
	}

	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(convResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode conversation detail: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "user" || out.Messages[0].Content != "hello" {
		t.Fatalf("messages[0] = %+v, want role=user content=%q", out.Messages[0], "hello")
	}
	if out.Messages[1].Role != "assistant" || out.Messages[1].Content != "the transcript works" {
		t.Fatalf("messages[1] = %+v, want role=assistant content=%q", out.Messages[1], "the transcript works")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail to compile**

Run: `go test ./api/... 2>&1 | head -30`
Expected: build failure — `api.NewServer` called with too few/too many arguments (the test files now pass an extra `store`/`realApp.Store()` that `NewServer`'s current 6-positional-arg-plus-hash signature doesn't accept), plus `store.CreateMessage`/`registry.MessageRole` compiling fine (Task 1 already provides those) but `MessageLister` and `s.messages` not existing yet.

- [ ] **Step 3: Implement the API changes in `api/server.go`**

Add this interface right after the existing `TaskLister` interface:

```go
// MessageLister is the message-transcript slice of registry.Store this
// package needs to extend conversation-detail (LOOM-18) with real chat
// turns instead of only task-lifecycle rows (LOOM-31) — satisfied
// structurally by any registry.Store, mirroring TaskLister.
type MessageLister interface {
	ListMessagesByConversation(ctx context.Context, conversationID string) ([]*registry.Message, error)
}
```

Add a field to the `Server` struct — change:

```go
type Server struct {
	dispatcher         Dispatcher
	sessions           SessionStore
	workspaces         WorkspaceLister
	tasks              TaskLister
	attachInfo         AttachInfoStore
	passwordHash       []byte
	sessionTTL         time.Duration
	loginThrottle      *loginThrottle
	streamPollInterval time.Duration
	mux                *http.ServeMux
}
```

to:

```go
type Server struct {
	dispatcher         Dispatcher
	sessions           SessionStore
	workspaces         WorkspaceLister
	tasks              TaskLister
	messages           MessageLister
	attachInfo         AttachInfoStore
	passwordHash       []byte
	sessionTTL         time.Duration
	loginThrottle      *loginThrottle
	streamPollInterval time.Duration
	mux                *http.ServeMux
}
```

Update `NewServer`'s signature and body — change:

```go
func NewServer(dispatcher Dispatcher, sessions SessionStore, workspaces WorkspaceLister, tasks TaskLister, attachInfo AttachInfoStore, passwordHash []byte, opts ...Option) *Server {
	s := &Server{
		dispatcher:         dispatcher,
		sessions:           sessions,
		workspaces:         workspaces,
		tasks:              tasks,
		attachInfo:         attachInfo,
		passwordHash:       passwordHash,
		sessionTTL:         defaultSessionTTL,
		loginThrottle:      newLoginThrottle(defaultLoginBackoffBase, defaultLoginBackoffMax),
		streamPollInterval: defaultStreamPollInterval,
	}
```

to:

```go
func NewServer(dispatcher Dispatcher, sessions SessionStore, workspaces WorkspaceLister, tasks TaskLister, messages MessageLister, attachInfo AttachInfoStore, passwordHash []byte, opts ...Option) *Server {
	s := &Server{
		dispatcher:         dispatcher,
		sessions:           sessions,
		workspaces:         workspaces,
		tasks:              tasks,
		messages:           messages,
		attachInfo:         attachInfo,
		passwordHash:       passwordHash,
		sessionTTL:         defaultSessionTTL,
		loginThrottle:      newLoginThrottle(defaultLoginBackoffBase, defaultLoginBackoffMax),
		streamPollInterval: defaultStreamPollInterval,
	}
```

Add `messageSummary` and extend `getConversationResponse` — change:

```go
type getConversationResponse struct {
	ConversationID string             `json:"conversation_id"`
	Tasks          []conversationTask `json:"tasks"`
}
```

to:

```go
type messageSummary struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TaskID    string    `json:"task_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type getConversationResponse struct {
	ConversationID string             `json:"conversation_id"`
	Tasks          []conversationTask `json:"tasks"`
	Messages       []messageSummary   `json:"messages"`
}
```

Update `handleGetConversation` — change:

```go
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}

	out := make([]conversationTask, 0)
	for _, t := range tasks {
		if t.ConversationID != id {
			continue
		}
		out = append(out, conversationTask{
			ID:          t.ID,
			WorkspaceID: t.WorkspaceID,
			Kind:        string(t.Kind),
			AgentType:   t.AgentType,
			Status:      string(t.Status),
			CreatedAt:   t.CreatedAt,
			UpdatedAt:   t.UpdatedAt,
			StartedAt:   t.StartedAt,
			CompletedAt: t.CompletedAt,
		})
	}
	if len(out) == 0 {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	}

	writeJSON(w, http.StatusOK, getConversationResponse{ConversationID: id, Tasks: out})
}
```

to:

```go
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}

	out := make([]conversationTask, 0)
	for _, t := range tasks {
		if t.ConversationID != id {
			continue
		}
		out = append(out, conversationTask{
			ID:          t.ID,
			WorkspaceID: t.WorkspaceID,
			Kind:        string(t.Kind),
			AgentType:   t.AgentType,
			Status:      string(t.Status),
			CreatedAt:   t.CreatedAt,
			UpdatedAt:   t.UpdatedAt,
			StartedAt:   t.StartedAt,
			CompletedAt: t.CompletedAt,
		})
	}

	messages, err := s.messages.ListMessagesByConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch conversation")
		return
	}
	msgOut := make([]messageSummary, 0, len(messages))
	for _, m := range messages {
		msgOut = append(msgOut, messageSummary{
			ID:        m.ID,
			Role:      string(m.Role),
			Content:   m.Content,
			TaskID:    m.TaskID,
			CreatedAt: m.CreatedAt,
		})
	}

	// A conversation is only truly unknown if it has neither task
	// history nor any logged messages — an answer_directly-only
	// conversation (LOOM-31) has messages but zero tasks, and must not
	// 404.
	if len(out) == 0 && len(msgOut) == 0 {
		writeError(w, http.StatusNotFound, "no such conversation")
		return
	}

	writeJSON(w, http.StatusOK, getConversationResponse{ConversationID: id, Tasks: out, Messages: msgOut})
}
```

Also update the doc comment directly above `handleGetConversation` (currently starting `// handleGetConversation returns a conversation's full task history —`) to mention messages — replace it with:

```go
// handleGetConversation returns a conversation's full task history (every
// Task row sharing this conversation_id, oldest first) plus its message
// transcript (LOOM-31: every Message row sharing this conversation_id,
// oldest first) — TaskLister.ListTasks's and MessageLister.
// ListMessagesByConversation's own orders, respectively. A conversation_id
// matching zero tasks AND zero messages is a 404, not an empty response —
// unlike handleListConversations, this is a "fetch one thing" endpoint. A
// conversation that only ever produced answer_directly replies has
// messages but no tasks, and must still resolve to 200.
```

- [ ] **Step 4: Update `cmd/loomuxd/main.go`**

Change:

```go
	server := api.NewServer(loomux, loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), apiCfg.PasswordHash, api.WithSessionTTL(apiCfg.SessionTTL))
```

to:

```go
	server := api.NewServer(loomux, loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), apiCfg.PasswordHash, api.WithSessionTTL(apiCfg.SessionTTL))
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./api/... -v -run 'TestGetConversation|TestIntegration' 2>&1 | tail -100`
Expected: every matching test PASSes, including the two new `server_test.go` cases and the new integration test, plus every pre-existing `TestGetConversation_*`/`TestIntegration_*` test (proving the widened 404 and the extra constructor parameter didn't break anything already covered).

Then run the full API suite: `go test ./api/...`
Expected: `ok`.

- [ ] **Step 6: Update `api/README.md`**

In the `GET /api/v1/conversations/{id}` bullet (in the "Layout" section's `server.go` list), change:

```
  - `GET /api/v1/conversations/{id}` — auth-gated (LOOM-18), the full task
    history for one conversation: `{conversation_id, tasks: [{id,
    workspace_id, kind, agent_type, status, created_at, updated_at,
    started_at, completed_at}, ...]}`, oldest first; `404` if no task
    matches that `conversation_id`. "History" here is exactly what the
    registry stores — task lifecycle rows, not a per-turn chat transcript
    (no message log exists in the schema). Both conversation endpoints
    share `TaskLister`, a narrow seam (`ListTasks`, the store's
    unfiltered, cross-workspace task query — `ListTasksByWorkspace` alone
    can't answer "every task in this conversation" since a conversation
    isn't pinned to one workspace) satisfied structurally by
    `*app.App.Store()`.
```

to:

```
  - `GET /api/v1/conversations/{id}` — auth-gated (LOOM-18), the full task
    history for one conversation plus its message transcript (LOOM-31):
    `{conversation_id, tasks: [{id, workspace_id, kind, agent_type,
    status, created_at, updated_at, started_at, completed_at}, ...],
    messages: [{id, role, content, task_id, created_at}, ...]}`, both
    oldest first; `404` only if there is neither task history nor any
    message for that `conversation_id` — an `answer_directly`-only
    conversation has messages but no tasks and still resolves to `200`.
    Backed by `TaskLister` (`ListTasks`, the store's unfiltered,
    cross-workspace task query — `ListTasksByWorkspace` alone can't answer
    "every task in this conversation" since a conversation isn't pinned to
    one workspace) and `MessageLister` (`ListMessagesByConversation`),
    both satisfied structurally by `*app.App.Store()`.
```

- [ ] **Step 7: Format, vet, full-repo verification, commit**

```bash
gofmt -l api/ cmd/
go vet ./...
go build ./...
go test ./...
```

Expected: `gofmt -l` prints nothing, `go vet`/`go build` succeed silently, `go test ./...` reports `ok` for every package.

```bash
git add api/server.go api/server_test.go api/integration_test.go cmd/loomuxd/main.go api/README.md
git commit -m "$(cat <<'EOF'
feat(api): expose message transcript via GET /conversations/{id} (LOOM-31)

Additive `messages` field alongside the existing `tasks` field. Widens
the 404 condition to "no tasks AND no messages" so an answer_directly-
only conversation (which has messages but never creates a task) is no
longer invisible.
EOF
)"
```
