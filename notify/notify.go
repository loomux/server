// Package notify tells the user about work that finished while they
// weren't looking (LOOM-102): a turn done, failed, or stopped on a prompt
// only they can answer. The one Notifier so far posts to an ntfy topic.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/internal/metrics"
)

// Kind is what happened.
type Kind string

const (
	KindDone     Kind = "done"
	KindFailed   Kind = "failed"
	KindNeedsYou Kind = "needs_you"
)

// AllKinds are every Kind, in the order they are documented.
var AllKinds = []Kind{KindDone, KindFailed, KindNeedsYou}

// Event is one notification.
type Event struct {
	Kind Kind
	// Workspace names where the work ran; empty when none did (a direct
	// answer from the router).
	Workspace string
	// Summary is the body: the reply's start, or what went wrong.
	Summary string
	// Link opens the conversation; empty when no public URL is known.
	Link string
}

// Notifier delivers an Event.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// maxBodyRunes bounds a notification body: a phone shows a few lines.
const maxBodyRunes = 300

// Ntfy posts events to an ntfy topic (https://ntfy.sh, or self-hosted).
type Ntfy struct {
	url    string
	token  string
	client *http.Client
}

// NewNtfy constructs an Ntfy posting to baseURL/topic, authenticating
// with token as a bearer token when it isn't empty.
func NewNtfy(baseURL, topic, token string) *Ntfy {
	return &Ntfy{
		url:    strings.TrimRight(baseURL, "/") + "/" + topic,
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Notify posts e: its summary as the message, a title naming the
// workspace and what happened, and the link as the click action.
func (n *Ntfy) Notify(ctx context.Context, e Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(truncate(e.Summary, maxBodyRunes)))
	if err != nil {
		return errors.New("notify: ntfy: invalid server URL")
	}
	req.Header.Set("Title", title(e))
	tag, priority := "white_check_mark", "default"
	switch e.Kind {
	case KindFailed:
		tag, priority = "x", "high"
	case KindNeedsYou:
		tag, priority = "raising_hand", "high"
	}
	req.Header.Set("Tags", tag)
	req.Header.Set("Priority", priority)
	if e.Link != "" {
		req.Header.Set("Click", e.Link)
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// A *url.Error's message holds the URL, and so the topic — on a
		// public server, the one secret guarding it. Keep only its cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("notify: ntfy: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("notify: ntfy answered %s", resp.Status)
	}
	return nil
}

func title(e Event) string {
	what := string(e.Kind)
	if e.Kind == KindNeedsYou {
		what = "needs you"
	}
	if e.Workspace == "" {
		return "Loomux: " + what
	}
	return "Loomux: " + e.Workspace + " " + what
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ParseKinds parses a comma-separated list of Kinds ("done,failed").
func ParseKinds(raw string) (map[Kind]bool, error) {
	out := make(map[Kind]bool)
	for _, part := range strings.Split(raw, ",") {
		k := Kind(strings.TrimSpace(part))
		if k == "" {
			continue
		}
		known := false
		for _, a := range AllKinds {
			known = known || a == k
		}
		if !known {
			return nil, fmt.Errorf("notify: unknown event kind %q (want done, failed or needs_you)", k)
		}
		out[k] = true
	}
	return out, nil
}

// FilterConfig configures a Filter.
type FilterConfig struct {
	// Events are the kinds opted into; any other is dropped.
	Events map[Kind]bool
	// Burst events may go out at once; after that one more every Refill.
	// KindNeedsYou is exempt: it is blocked work, so it is never held.
	Burst  int
	Refill time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// AfterFunc schedules a digest's delivery; nil means time.AfterFunc.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
	// Logger and Metrics hear of digests; nil means neither.
	Logger  *slog.Logger
	Metrics *metrics.Metrics
}

// Outcome is what a Filter did with an event.
type Outcome string

const (
	// Sent: delivered now (or tried: see Notify's error).
	Sent Outcome = "sent"
	// Held: the rate limit is spent, so it waits for the next digest.
	Held Outcome = "held"
	// OptedOut: its kind isn't opted into.
	OptedOut Outcome = "opted_out"
)

// digestTimeout bounds a digest's delivery.
const digestTimeout = 15 * time.Second

// Filter passes events of the opted-in kinds on to a Notifier, at a
// bounded rate (a token bucket), so a burst of failures can't flood the
// user's phone. An event over the rate isn't lost (LOOM-249): it is held,
// and when the next token comes the held ones go out as one digest.
// KindNeedsYou bypasses the bucket.
type Filter struct {
	next   Notifier
	cfg    FilterConfig
	mu     sync.Mutex
	tokens float64
	last   time.Time
	// held are the events waiting on the next digest, and stop cancels
	// its timer; nil while none is scheduled.
	held   []Event
	stop   func() bool
	closed bool
	// sending counts digests on their way, for Close to wait on.
	sending sync.WaitGroup
}

// NewFilter wraps next.
func NewFilter(next Notifier, cfg FilterConfig) *Filter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	return &Filter{next: next, cfg: cfg, tokens: float64(cfg.Burst), last: cfg.Now()}
}

// Notify sends e unless its kind isn't opted into or the rate limit is
// spent, in which case e is held for the next digest. The error is the
// delivery's, when it was Sent.
func (f *Filter) Notify(ctx context.Context, e Event) (Outcome, error) {
	if !f.cfg.Events[e.Kind] {
		return OptedOut, nil
	}
	if e.Kind != KindNeedsYou && !f.takeOrHold(e) {
		return Held, nil
	}
	return Sent, f.next.Notify(ctx, e)
}

// takeOrHold takes a token for e and reports true, or holds e for the
// next digest and reports false. While events are held, a new one joins
// them rather than overtake them.
func (f *Filter) takeOrHold(e Event) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Once closed there is no digest to come: what remains goes now.
	if f.closed || len(f.held) == 0 && f.take() {
		return true
	}
	f.held = append(f.held, e)
	f.schedule()
	return false
}

// take refills the bucket and takes a token, if there is one. f.mu is
// held.
func (f *Filter) take() bool {
	now := f.cfg.Now()
	if f.cfg.Refill > 0 {
		f.tokens += float64(now.Sub(f.last)) / float64(f.cfg.Refill)
	}
	f.tokens = min(f.tokens, float64(f.cfg.Burst))
	f.last = now
	if f.tokens < 1 {
		return false
	}
	f.tokens--
	return true
}

// schedule arms the digest's timer for when the next token comes, unless
// it is armed or the filter is closed. f.mu is held.
func (f *Filter) schedule() {
	if f.stop != nil || f.closed {
		return
	}
	wait := time.Duration((1 - f.tokens) * float64(f.cfg.Refill))
	f.stop = f.cfg.AfterFunc(max(wait, 0), f.flush)
}

// flush sends the held events' digest once a token is there, or waits
// for the next.
func (f *Filter) flush() {
	f.mu.Lock()
	f.stop = nil
	if f.closed || len(f.held) == 0 {
		f.mu.Unlock()
		return
	}
	if !f.take() {
		f.schedule()
		f.mu.Unlock()
		return
	}
	held := f.held
	f.held = nil
	f.sending.Add(1)
	f.mu.Unlock()
	defer f.sending.Done()
	f.sendDigest(held, "")
}

// Close sends any held events' digest now, not waiting for a token, so
// none is lost when Loomux stops, and waits for a digest on its way. A
// nil Filter is closed.
func (f *Filter) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.closed = true
	if f.stop != nil {
		f.stop()
		f.stop = nil
	}
	held := f.held
	f.held = nil
	f.mu.Unlock()
	if len(held) > 0 {
		f.sendDigest(held, "shutting down")
	}
	f.sending.Wait()
}

// sendDigest delivers held as one event: the event itself when there is
// one, else a digest of them.
func (f *Filter) sendDigest(held []Event, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), digestTimeout)
	defer cancel()
	e := digest(held)
	err := f.next.Notify(ctx, e)
	outcome := metrics.NotifyDigestSent
	if err != nil {
		outcome = metrics.NotifyDigestError
	}
	f.cfg.Metrics.RecordNotification(string(e.Kind), outcome)
	if f.cfg.Logger == nil {
		return
	}
	log := f.cfg.Logger.With("kind", string(e.Kind), "events", len(held))
	if why != "" {
		log = log.With("reason", why)
	}
	if err != nil {
		log.Warn("notification digest not delivered", "error", err)
		return
	}
	log.Info("notification digest sent")
}

// digest is one event standing for held: held[0] when it is alone. A
// digest is a failure if any was, and keeps the workspace and link they
// share.
func digest(held []Event) Event {
	if len(held) == 1 {
		return held[0]
	}
	e := Event{Kind: KindDone, Workspace: held[0].Workspace, Link: held[0].Link}
	counts := make(map[Kind]int)
	for _, h := range held {
		counts[h.Kind]++
		if h.Kind == KindFailed {
			e.Kind = KindFailed
		}
		if h.Workspace != e.Workspace {
			e.Workspace = ""
		}
		if h.Link != e.Link {
			e.Link = ""
		}
	}
	var parts []string
	for _, k := range AllKinds {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	e.Summary = fmt.Sprintf("%d more turns ended while notifications were held back (%s). Open Loomux to see them.",
		len(held), strings.Join(parts, ", "))
	return e
}
