package notify_test

import (
	"context"
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

func TestFilterEventsAndRate(t *testing.T) {
	rec := &recorder{}
	now := time.Unix(1000, 0)
	f := notify.NewFilter(rec, notify.FilterConfig{
		Events: map[notify.Kind]bool{notify.KindDone: true, notify.KindFailed: true},
		Burst:  2,
		Refill: time.Minute,
		Now:    func() time.Time { return now },
	})
	ctx := context.Background()
	send := func(k notify.Kind) bool { sent, _ := f.Notify(ctx, notify.Event{Kind: k}); return sent }

	if send(notify.KindNeedsYou) {
		t.Errorf("an event kind not opted into was sent")
	}
	if !send(notify.KindDone) || !send(notify.KindFailed) {
		t.Fatalf("the first two events within the burst weren't sent")
	}
	if send(notify.KindDone) {
		t.Errorf("a third event inside a minute was sent past a burst of 2")
	}
	now = now.Add(time.Minute)
	if !send(notify.KindDone) {
		t.Errorf("an event a minute later wasn't sent")
	}
	if len(rec.events) != 3 {
		t.Errorf("%d events delivered, want 3", len(rec.events))
	}
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
