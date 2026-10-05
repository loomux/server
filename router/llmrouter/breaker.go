package llmrouter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Circuit breaker for the primary tier (LOOM-107): during a provider
// outage every message would otherwise wait out the primary's full
// timeout before escalating. After breakerThreshold consecutive calls
// that couldn't reach the primary, it's skipped for breakerCooldown;
// then one call tries it again, and a success closes the breaker.
// Only an unreachable or failing provider counts: a reply that was
// merely invalid shows the model is up.
const (
	breakerThreshold = 3
	breakerCooldown  = 2 * time.Minute
)

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time
}

func newBreaker() *breaker { return &breaker{now: time.Now} }

// allow reports whether the primary should be tried now. Once the
// cooldown has passed it lets calls through again; the next failure
// reopens it at once, since failures is still past the threshold.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.now().Before(b.openUntil)
}

// record notes a primary call's outcome and reports whether that call
// opened the breaker.
func (b *breaker) record(unavailable bool) (opened bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !unavailable {
		b.failures = 0
		b.openUntil = time.Time{}
		return false
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = b.now().Add(breakerCooldown)
		return true
	}
	return false
}

// unavailableError marks a tier call that never got a usable answer
// from the provider: transport failure, HTTP error, timeout, no choices.
// The breaker counts these; invalid tool calls it doesn't.
type unavailableError struct{ err error }

func (e *unavailableError) Error() string { return e.err.Error() }
func (e *unavailableError) Unwrap() error { return e.err }

func isUnavailable(err error) bool {
	var u *unavailableError
	return errors.As(err, &u)
}

// errPrimarySkipped stands in for the primary's error while its breaker
// is open.
var errPrimarySkipped = errors.New("skipped: the primary tier failed repeatedly and is resting")

// skipPrimary reports whether to go straight to escalation: only with an
// escalation tier to go to, and while the breaker is open.
func (m *Model) skipPrimary() bool {
	return m.cfg.Escalation != nil && !m.primaryBreaker.allow()
}

// recordPrimary feeds a primary call's outcome to the breaker. A call the
// caller cancelled says nothing about the provider.
func (m *Model) recordPrimary(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if m.primaryBreaker.record(err != nil && isUnavailable(err)) {
		slog.Warn("router primary tier failing; skipping it", "consecutive_failures", breakerThreshold, "for", breakerCooldown.String(), "error", err)
		m.metrics.SetRouterBreakerOpen(true)
	} else if err == nil || !isUnavailable(err) {
		m.metrics.SetRouterBreakerOpen(false)
	}
}
