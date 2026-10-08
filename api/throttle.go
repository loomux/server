package api

import (
	"sync"
	"time"
)

// loginThrottle implements exponential backoff on repeated failed
// /login attempts. Global — not scoped per-IP or per-user — deliberately:
// this is a single-user system with no username to scope by (loginRequest
// is just {password}), and the confirmed deployment model (a reverse
// proxy in front, per LOOM-9) means every request's RemoteAddr is
// typically the proxy's own loopback address anyway, so per-IP scoping
// wouldn't provide real isolation here without also trusting a
// client-spoofable X-Forwarded-For header — a global counter sidesteps
// that trust question entirely.
//
// Backoff, not a hard lockout, on purpose: a lockout on a single-account
// system is itself a denial-of-service vector — an attacker who can't
// guess the password can still lock the real user out just by failing
// repeatedly. Backoff only ever slows an attempt down (bounded by max);
// it never permanently blocks the legitimate user, who can always get in
// by waiting.
//
// State is in-memory only — a restart clears it. Acceptable for v1: the
// confirmed deployment model has no attacker-triggerable restart path.
type loginThrottle struct {
	mu            sync.Mutex
	failureCount  int
	lastFailureAt time.Time
	// checking is set while an admitted attempt's password is being
	// checked: concurrent attempts would otherwise all pass the backoff
	// before any of them recorded its failure (LOOM-142).
	checking bool

	base time.Duration
	max  time.Duration
}

func newLoginThrottle(base, max time.Duration) *loginThrottle {
	return &loginThrottle{base: base, max: max}
}

// wait returns how much longer the caller must wait before another
// login attempt is allowed; zero means proceed now.
func (t *loginThrottle) wait() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.failureCount == 0 {
		return 0
	}
	elapsed := time.Since(t.lastFailureAt)
	delay := t.delayForLocked()
	if elapsed >= delay {
		return 0
	}
	return delay - elapsed
}

// admit reserves the right to check one password: zero means go ahead,
// and the caller must then end with recordFailure, recordSuccess or
// release. Otherwise it is how long to wait — the backoff, or a second
// while another attempt is being checked.
func (t *loginThrottle) admit() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.checking {
		return time.Second
	}
	if t.failureCount > 0 {
		if left := t.delayForLocked() - time.Since(t.lastFailureAt); left > 0 {
			return left
		}
	}
	t.checking = true
	return 0
}

// release ends an admitted attempt that reached no verdict.
func (t *loginThrottle) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checking = false
}

// delayForLocked computes the current backoff delay: base * 2^(n-1),
// capped at max. A zero base (tests only — production always sets one)
// legitimately means "no delay," so only a negative result (shift
// overflow) is treated as "clamp to max," not a zero one. Callers must
// hold t.mu.
func (t *loginThrottle) delayForLocked() time.Duration {
	n := t.failureCount - 1
	if n > 30 { // far past where we'd already be clamped to max; avoids
		n = 30 // any concern about the shift amount itself
	}
	delay := t.base * time.Duration(1<<uint(n))
	if delay < 0 { // overflow
		return t.max
	}
	if delay > t.max {
		return t.max
	}
	return delay
}

func (t *loginThrottle) recordFailure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failureCount++
	t.lastFailureAt = time.Now()
	t.checking = false
}

func (t *loginThrottle) recordSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failureCount = 0
	t.checking = false
}
