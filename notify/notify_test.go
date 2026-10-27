package notify_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/notify"
)

type captured struct {
	path, body string
	header     http.Header
}

func ntfyServer(t *testing.T, status int) (*httptest.Server, func() []captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{path: r.URL.Path, body: string(body), header: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		return append([]captured(nil), got...)
	}
}

func TestNtfyNotify(t *testing.T) {
	srv, got := ntfyServer(t, http.StatusOK)
	n := notify.NewNtfy(srv.URL+"/", "loomux", "tk_secret")
	err := n.Notify(context.Background(), notify.Event{
		Kind: notify.KindFailed, Workspace: "theWyseKube", Summary: "the agent exited", Link: "https://loomux.example/conversations/c1",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/loomux" || r.body != "the agent exited" {
		t.Errorf("path %q body %q", r.path, r.body)
	}
	for k, want := range map[string]string{
		"Title":         "Loomux: theWyseKube failed",
		"Click":         "https://loomux.example/conversations/c1",
		"Authorization": "Bearer tk_secret",
		"Priority":      "high",
		"Tags":          "x",
	} {
		if v := r.header.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
}

func TestNtfyTitles(t *testing.T) {
	for _, tc := range []struct {
		e     notify.Event
		title string
	}{
		{notify.Event{Kind: notify.KindDone, Workspace: "ws"}, "Loomux: ws done"},
		{notify.Event{Kind: notify.KindNeedsYou, Workspace: "ws"}, "Loomux: ws needs you"},
		{notify.Event{Kind: notify.KindDone}, "Loomux: done"},
	} {
		srv, got := ntfyServer(t, http.StatusOK)
		if err := notify.NewNtfy(srv.URL, "t", "").Notify(context.Background(), tc.e); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		r := got()[0]
		if r.header.Get("Title") != tc.title {
			t.Errorf("title = %q, want %q", r.header.Get("Title"), tc.title)
		}
		if r.header.Get("Authorization") != "" || r.header.Get("Click") != "" {
			t.Errorf("unexpected auth/click headers: %v", r.header)
		}
	}
}

func TestNtfyLongBodyTruncated(t *testing.T) {
	srv, got := ntfyServer(t, http.StatusOK)
	long := strings.Repeat("é", 1000)
	if err := notify.NewNtfy(srv.URL, "t", "").Notify(context.Background(), notify.Event{Kind: notify.KindDone, Summary: long}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	body := got()[0].body
	if n := len([]rune(body)); n > 301 || !strings.HasSuffix(body, "…") {
		t.Errorf("body is %d runes, ends %q; want at most 300 + an ellipsis", n, body[len(body)-3:])
	}
}

func TestNtfyServerError(t *testing.T) {
	srv, _ := ntfyServer(t, http.StatusForbidden)
	if err := notify.NewNtfy(srv.URL, "t", "").Notify(context.Background(), notify.Event{Kind: notify.KindDone}); err == nil {
		t.Fatalf("Notify against a 403: err = nil")
	}
}

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Notify(ctx context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

// fakeTimer stands in for time.AfterFunc: a test fires it by hand.
type fakeTimer struct {
	mu    sync.Mutex
	f     func()
	after time.Duration
}

func (ft *fakeTimer) AfterFunc(d time.Duration, f func()) func() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.f, ft.after = f, d
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		armed := ft.f != nil
		ft.f = nil
		return armed
	}
}

// fire runs the armed timer's function, reporting false when none is.
func (ft *fakeTimer) fire() bool {
	ft.mu.Lock()
	f := ft.f
	ft.f = nil
	ft.mu.Unlock()
	if f == nil {
		return false
	}
	f()
	return true
}

func (r *recorder) got() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

func newTestFilter(burst int, now *time.Time, timer *fakeTimer, kinds ...notify.Kind) (*notify.Filter, *recorder) {
	rec := &recorder{}
	events := make(map[notify.Kind]bool)
	for _, k := range kinds {
		events[k] = true
	}
	return notify.NewFilter(rec, notify.FilterConfig{
		Events: events, Burst: burst, Refill: time.Minute,
		Now:       func() time.Time { return *now },
		AfterFunc: timer.AfterFunc,
	}), rec
}

func TestFilterEventsAndRate(t *testing.T) {
	now := time.Unix(1000, 0)
	timer := &fakeTimer{}
	f, rec := newTestFilter(2, &now, timer, notify.KindDone, notify.KindFailed)
	ctx := context.Background()
	send := func(k notify.Kind) notify.Outcome { o, _ := f.Notify(ctx, notify.Event{Kind: k}); return o }

	if o := send(notify.KindNeedsYou); o != notify.OptedOut {
		t.Errorf("an event kind not opted into: %s, want opted_out", o)
	}
	if send(notify.KindDone) != notify.Sent || send(notify.KindFailed) != notify.Sent {
		t.Fatalf("the first two events within the burst weren't sent")
	}
	if o := send(notify.KindDone); o != notify.Held {
		t.Errorf("a third event inside a minute: %s, want held past a burst of 2", o)
	}
	if timer.after != time.Minute {
		t.Errorf("digest scheduled in %s, want a minute (the next token)", timer.after)
	}
	now = now.Add(time.Minute)
	if !timer.fire() {
		t.Fatal("no digest scheduled for the held event")
	}
	if len(rec.got()) != 3 {
		t.Errorf("%d events delivered, want 3: none lost", len(rec.got()))
	}
	if send(notify.KindDone) != notify.Held {
		t.Errorf("an event right after the digest took the token wasn't held")
	}
}

// LOOM-249: a needs-you in the middle of a burst goes out at once, never
// held behind the rate limit, and doesn't spend a token.
func TestFilterNeedsYouBypassesRate(t *testing.T) {
	now := time.Unix(1000, 0)
	timer := &fakeTimer{}
	f, rec := newTestFilter(2, &now, timer, notify.KindDone, notify.KindNeedsYou)
	ctx := context.Background()
	for i, k := range []notify.Kind{notify.KindDone, notify.KindDone, notify.KindDone, notify.KindNeedsYou, notify.KindDone, notify.KindNeedsYou} {
		o, err := f.Notify(ctx, notify.Event{Kind: k, Summary: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		want := notify.Sent
		if k == notify.KindDone && i >= 2 {
			want = notify.Held
		}
		if o != want {
			t.Errorf("event %d (%s): %s, want %s", i, k, o, want)
		}
	}
	var needsYou int
	for _, e := range rec.got() {
		if e.Kind == notify.KindNeedsYou {
			needsYou++
		}
	}
	if needsYou != 2 || len(rec.got()) != 4 {
		t.Fatalf("delivered %+v; want both needs-you at once, beside the 2 in the burst", rec.got())
	}
	now = now.Add(time.Minute)
	timer.fire()
	got := rec.got()
	if len(got) != 5 || !strings.HasPrefix(got[4].Summary, "2 more turns ended") {
		t.Fatalf("after a minute: %+v; want one digest of the 2 held", got)
	}
}

// LOOM-249: events over the rate are coalesced into one digest at the
// next token: a failure if any held one was, keeping the workspace and
// link they share. Events arriving while some are held join them rather
// than overtake them.
func TestFilterCoalescesHeld(t *testing.T) {
	now := time.Unix(1000, 0)
	timer := &fakeTimer{}
	f, rec := newTestFilter(1, &now, timer, notify.KindDone, notify.KindFailed)
	ctx := context.Background()
	link := "https://loomux.example/conversations/c"
	f.Notify(ctx, notify.Event{Kind: notify.KindDone, Workspace: "ws", Summary: "first", Link: link})
	for _, k := range []notify.Kind{notify.KindDone, notify.KindFailed, notify.KindDone} {
		if o, _ := f.Notify(ctx, notify.Event{Kind: k, Workspace: "ws", Summary: "secret reply", Link: link}); o != notify.Held {
			t.Fatalf("%s: %s, want held", k, o)
		}
	}
	// The timer fired a little early: no token yet, so it waits again.
	now = now.Add(59 * time.Second)
	timer.fire()
	if len(rec.got()) != 1 {
		t.Fatalf("a digest went out before the next token")
	}
	if timer.after != time.Second {
		t.Errorf("rescheduled in %s, want the second left to the token", timer.after)
	}
	now = now.Add(time.Second)
	if !timer.fire() {
		t.Fatal("no digest rescheduled")
	}
	got := rec.got()
	if len(got) != 2 {
		t.Fatalf("delivered %d, want the first and one digest", len(got))
	}
	want := notify.Event{Kind: notify.KindFailed, Workspace: "ws", Link: link,
		Summary: "3 more turns ended while notifications were held back (2 done, 1 failed). Open Loomux to see them."}
	if got[1] != want {
		t.Errorf("digest = %+v\nwant     %+v", got[1], want)
	}

	// Held events from different places: the digest names neither.
	now = now.Add(time.Hour)
	f.Notify(ctx, notify.Event{Kind: notify.KindDone, Workspace: "a", Link: link})
	f.Notify(ctx, notify.Event{Kind: notify.KindDone, Workspace: "a", Link: link})
	f.Notify(ctx, notify.Event{Kind: notify.KindDone, Workspace: "b", Link: link + "2"})
	now = now.Add(time.Minute)
	timer.fire()
	got = rec.got()
	if d := got[len(got)-1]; d.Kind != notify.KindDone || d.Workspace != "" || d.Link != "" {
		t.Errorf("mixed digest = %+v; want done, no workspace or link", d)
	}

	// One held event goes out as itself.
	now = now.Add(time.Hour)
	f.Notify(ctx, notify.Event{Kind: notify.KindDone, Summary: "x"})
	f.Notify(ctx, notify.Event{Kind: notify.KindFailed, Summary: "alone"})
	now = now.Add(time.Minute)
	timer.fire()
	got = rec.got()
	if d := got[len(got)-1]; d != (notify.Event{Kind: notify.KindFailed, Summary: "alone"}) {
		t.Errorf("a lone held event went out as %+v", d)
	}
}

// LOOM-249: events still held when Loomux stops go out as a digest
// rather than vanish, and the timer is stopped.
func TestFilterCloseSendsHeld(t *testing.T) {
	now := time.Unix(1000, 0)
	timer := &fakeTimer{}
	f, rec := newTestFilter(1, &now, timer, notify.KindDone)
	ctx := context.Background()
	for range 3 {
		f.Notify(ctx, notify.Event{Kind: notify.KindDone})
	}
	f.Close()
	got := rec.got()
	if len(got) != 2 || !strings.HasPrefix(got[1].Summary, "2 more turns ended") {
		t.Fatalf("delivered %+v; want the first and a digest of 2", got)
	}
	if timer.fire() {
		t.Error("the digest timer was still armed after Close")
	}
	if o, _ := f.Notify(ctx, notify.Event{Kind: notify.KindDone}); o != notify.Sent {
		t.Errorf("after Close an event was %s: with no digest to come, it must go now", o)
	}
	var nilFilter *notify.Filter
	nilFilter.Close()
}

func TestParseKinds(t *testing.T) {
	got, err := notify.ParseKinds(" done, needs_you ")
	if err != nil || !got[notify.KindDone] || !got[notify.KindNeedsYou] || got[notify.KindFailed] {
		t.Fatalf("ParseKinds = %v, %v", got, err)
	}
	if _, err := notify.ParseKinds("done,finished"); err == nil {
		t.Fatalf("ParseKinds accepted an unknown kind")
	}
}

// On a public ntfy server the topic is the secret: a delivery error must
// not carry the URL (LOOM-102 review).
func TestNtfyErrorHidesTopic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	err := notify.NewNtfy(url, "s3cret-topic", "").Notify(context.Background(), notify.Event{Kind: notify.KindDone})
	if err == nil || strings.Contains(err.Error(), "s3cret-topic") {
		t.Fatalf("err = %v, want a delivery error without the topic", err)
	}
}
