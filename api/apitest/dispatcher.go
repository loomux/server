package apitest

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
)

// Dispatch is one message a client submitted, as the Dispatcher's
// handler sees it.
type Dispatch struct {
	// ID is the dispatch's id, the dispatch_id the client was answered
	// with.
	ID             string
	ConversationID string
	Message        string
	WorkspaceHint  string
	// ConfirmationID names the offer the message answers, if any.
	ConfirmationID string
}

// Handler answers one dispatch in place of the router: its string is the
// reply, its error the dispatch's failure. ctx is cancelled when the
// client cancels the dispatch or the server shuts down.
type Handler func(ctx context.Context, d Dispatch) (reply string, err error)

// Dispatcher stands in for the router. Each POST /api/v1/dispatch runs
// as a real dispatch job (queued, running, then succeeded or failed, with
// idempotency keys, one job per conversation and cancellation as in
// production); what the job does is the handler's. Until Handle is
// called, every dispatch succeeds with DefaultReply.
//
// A successful reply is also logged to the conversation as an assistant
// message, as the router does, so GET /conversations/{id} shows it and
// the stream announces it.
type Dispatcher struct {
	jobs  *dispatch.Service
	store registry.Store

	mu      sync.Mutex
	handler Handler
	seen    []Dispatch
}

// DefaultReply is what a dispatch answers until Handle says otherwise.
const DefaultReply = "ok"

func newDispatcher(store registry.Store) *Dispatcher {
	d := &Dispatcher{store: store}
	d.jobs = dispatch.New(store, d.run)
	return d
}

func (d *Dispatcher) run(ctx context.Context, job *registry.Dispatch) (string, error) {
	seen := Dispatch{ID: job.ID, ConversationID: job.ConversationID, Message: job.Message,
		WorkspaceHint: job.WorkspaceHint, ConfirmationID: job.ConfirmationID}
	d.mu.Lock()
	d.seen = append(d.seen, seen)
	handler := d.handler
	d.mu.Unlock()

	reply := DefaultReply
	if handler != nil {
		var err error
		if reply, err = handler(ctx, seen); err != nil {
			return "", err
		}
	}
	msg := &registry.Message{ID: uuid.NewString(), ConversationID: job.ConversationID, DispatchID: job.ID,
		Origin: registry.MessageOriginNone, Role: registry.MessageRoleAssistant, Content: reply}
	if err := d.store.CreateMessage(context.WithoutCancel(ctx), msg); err != nil {
		return "", err
	}
	return reply, nil
}

// Handle sets how every later dispatch is answered; nil restores
// DefaultReply.
func (d *Dispatcher) Handle(h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handler = h
}

// Reply makes every later dispatch succeed with text.
func (d *Dispatcher) Reply(text string) {
	d.Handle(func(context.Context, Dispatch) (string, error) { return text, nil })
}

// Fail makes every later dispatch fail with err: the job ends failed,
// its error the client's to show.
func (d *Dispatcher) Fail(err error) {
	d.Handle(func(context.Context, Dispatch) (string, error) { return "", err })
}

// Block makes every later dispatch stay running until release is called
// (it then succeeds with DefaultReply) or the dispatch is cancelled (it
// then fails with the cancellation). It is how a test holds a
// conversation busy. Calling release more than once is harmless.
func (d *Dispatcher) Block() (release func()) {
	gate := make(chan struct{})
	var once sync.Once
	d.Handle(func(ctx context.Context, _ Dispatch) (string, error) {
		select {
		case <-gate:
			return DefaultReply, nil
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	})
	return func() { once.Do(func() { close(gate) }) }
}

// Dispatches returns every dispatch that started running so far, oldest
// first.
func (d *Dispatcher) Dispatches() []Dispatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Dispatch(nil), d.seen...)
}
