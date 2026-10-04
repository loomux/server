// Package notify tells the user about work that finished while they
// weren't looking (LOOM-102): a turn done, failed, or stopped on a prompt
// only they can answer. The one Notifier so far posts to an ntfy topic.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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
		return fmt.Errorf("notify: ntfy: %w", err)
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
	Burst  int
	Refill time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Filter passes events of the opted-in kinds on to a Notifier, at a
// bounded rate (a token bucket), so a burst of failures can't flood the
// user's phone.
type Filter struct {
	next   Notifier
	cfg    FilterConfig
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewFilter wraps next.
func NewFilter(next Notifier, cfg FilterConfig) *Filter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Filter{next: next, cfg: cfg, tokens: float64(cfg.Burst), last: cfg.Now()}
}

// Notify sends e unless its kind isn't opted into or the rate limit is
// spent, reporting whether it was sent.
func (f *Filter) Notify(ctx context.Context, e Event) (bool, error) {
	if !f.cfg.Events[e.Kind] || !f.take() {
		return false, nil
	}
	return true, f.next.Notify(ctx, e)
}

func (f *Filter) take() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
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
