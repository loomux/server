package api_test

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
)

// fullReadCounter counts the whole-table and whole-conversation reads a
// stream used to make on every poll.
type fullReadCounter struct {
	registry.Store
	full atomic.Int32
}

func (c *fullReadCounter) ListTasks(ctx context.Context) ([]*registry.Task, error) {
	c.full.Add(1)
	return c.Store.ListTasks(ctx)
}

func (c *fullReadCounter) ListMessagesByConversation(ctx context.Context, id string) ([]*registry.Message, error) {
	c.full.Add(1)
	return c.Store.ListMessagesByConversation(ctx, id)
}

func (c *fullReadCounter) ListDispatchesByConversation(ctx context.Context, id string) ([]*registry.Dispatch, error) {
	c.full.Add(1)
	return c.Store.ListDispatchesByConversation(ctx, id)
}

// LOOM-145: an open stream polls only its own conversation's tasks and
// what's new since its last poll, never the whole tasks table or the
// whole conversation, and it still reports every turn.
func TestStream_PollsOnlyWhatsNew(t *testing.T) {
	counter := &fullReadCounter{Store: newTestStore(t)}
	release := make(chan struct{}, 2)
	jobs := dispatch.New(counter, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		<-release
		return "reply to " + d.Message, nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jobs.Shutdown(ctx)
	})
	srv := httptest.NewServer(api.NewServer(jobs, counter, counter, counter, counter, counter, counter, []byte(testPasswordHash(t)),
		api.WithStreamPollInterval(10*time.Millisecond)))
	t.Cleanup(srv.Close)
	token, _ := login(t, srv.URL, testPassword)

	r := openStream(t, srv.URL, token, "c-new")
	time.Sleep(50 * time.Millisecond)
	counter.full.Store(0)

	for _, msg := range []string{"one", "two"} {
		_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-new", "message": msg}, nil)
		release <- struct{}{}
		for {
			u := readDispatchUpdate(t, r)
			if u.DispatchID == d.DispatchID && u.Status == "succeeded" {
				if u.Reply != "reply to "+msg {
					t.Fatalf("turn %q: reply %q", msg, u.Reply)
				}
				break
			}
		}
	}
	time.Sleep(50 * time.Millisecond) // a few more polls
	if n := counter.full.Load(); n != 0 {
		t.Fatalf("the stream made %d full reads after connecting, want 0", n)
	}
}
