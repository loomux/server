package api

import (
	"testing"
	"time"
)

func TestLoginThrottle_NoFailures_NoWait(t *testing.T) {
	th := newLoginThrottle(time.Second, 30*time.Second)
	if w := th.wait(); w != 0 {
		t.Errorf("wait() = %v, want 0 before any failure", w)
	}
}

func TestLoginThrottle_AfterFailure_WaitsAtLeastBase(t *testing.T) {
	th := newLoginThrottle(50*time.Millisecond, 30*time.Second)
	th.recordFailure()

	w := th.wait()
	if w <= 0 || w > 50*time.Millisecond {
		t.Errorf("wait() = %v, want a positive value <= base (50ms)", w)
	}

	time.Sleep(60 * time.Millisecond)
	if w := th.wait(); w != 0 {
		t.Errorf("wait() = %v, want 0 once the base delay has elapsed", w)
	}
}

func TestLoginThrottle_ExponentialGrowth_CapsAtMax(t *testing.T) {
	th := newLoginThrottle(10*time.Millisecond, 40*time.Millisecond)
	for i := 0; i < 20; i++ {
		th.recordFailure()
	}
	// 20 failures would be far past the cap without clamping (10ms *
	// 2^19 is enormous) — must still be bounded by max.
	if w := th.wait(); w > 40*time.Millisecond {
		t.Errorf("wait() = %v, want capped at max (40ms)", w)
	}
}

func TestLoginThrottle_ZeroBase_NeverWaits(t *testing.T) {
	th := newLoginThrottle(0, time.Second)
	for i := 0; i < 5; i++ {
		th.recordFailure()
	}
	if w := th.wait(); w != 0 {
		t.Errorf("wait() = %v, want 0 — a zero base means no delay, not \"clamp to max\"", w)
	}
}

func TestLoginThrottle_SuccessResetsCounter(t *testing.T) {
	th := newLoginThrottle(50*time.Millisecond, 30*time.Second)
	th.recordFailure()
	th.recordFailure()
	th.recordFailure()

	th.recordSuccess()

	if w := th.wait(); w != 0 {
		t.Errorf("wait() = %v, want 0 immediately after a recorded success", w)
	}

	// And the backoff restarts from the base delay, not wherever it left
	// off — proving recordSuccess actually resets state, not just
	// happens to return 0 once.
	th.recordFailure()
	w := th.wait()
	if w <= 0 || w > 50*time.Millisecond {
		t.Errorf("wait() after one failure post-reset = %v, want a positive value <= base (50ms), not wherever the pre-reset backoff had grown to", w)
	}
}

// An admitted attempt holds the throttle until its verdict: a second
// attempt meanwhile waits, even with no failures yet (LOOM-142).
func TestLoginThrottle_AdmitReservesUntilVerdict(t *testing.T) {
	th := newLoginThrottle(time.Hour, time.Hour)
	if w := th.admit(); w != 0 {
		t.Fatalf("first admit() = %v, want 0", w)
	}
	if w := th.admit(); w <= 0 {
		t.Fatal("second admit() while the first is checked = 0, want a wait")
	}
	th.release()
	if w := th.admit(); w != 0 {
		t.Fatalf("admit() after release = %v, want 0", w)
	}
	th.recordFailure()
	if w := th.admit(); w <= 0 {
		t.Fatal("admit() inside the backoff = 0, want a wait")
	}
}

// A device token is only good under the password hash it was minted
// with, and only unaltered (LOOM-151).
func TestLoginDevices_TokenBoundToPasswordHash(t *testing.T) {
	d := newLoginDevices([]byte("hash-1"), time.Second, time.Minute)
	tok, err := d.issue()
	if err != nil {
		t.Fatal(err)
	}
	if d.throttle(tok) == nil {
		t.Fatal("a token this server issued was not recognised")
	}
	if d.throttle(tok) != d.throttle(tok) {
		t.Fatal("one device got two throttles")
	}
	if newLoginDevices([]byte("hash-2"), time.Second, time.Minute).throttle(tok) != nil {
		t.Fatal("a token survived a password change")
	}
	// Flip a bit mid-MAC: the last character's low bits are base64
	// padding, and a flip there could decode to the same bytes.
	flipped := []byte(tok)
	flipped[len(flipped)-5] ^= 1
	for _, bad := range []string{"", "x", string(flipped), tok + "A"} {
		if d.throttle(bad) != nil {
			t.Errorf("token %q was accepted", bad)
		}
	}
}
