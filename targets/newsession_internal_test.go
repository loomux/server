package targets

import (
	"context"
	"errors"
	"testing"
)

// tmux stops its server when the last session closes; a new-session
// racing that shutdown fails with "server exited unexpectedly". That's
// not a real failure: one retry starts a fresh server.
func TestNewSessionRetriesAServerShuttingDown(t *testing.T) {
	calls := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New(`exec: "tmux new-session": server exited unexpectedly`)
		}
		return "", nil
	}
	if err := newSession(context.Background(), run, "s", "/tmp", "sh"); err != nil || calls != 2 {
		t.Fatalf("newSession = %v after %d calls, want success on the retry", err, calls)
	}

	// Any other error, or the same one twice, is returned.
	for _, errs := range [][]error{
		{errors.New("duplicate session: s")},
		{errors.New("server exited unexpectedly"), errors.New("server exited unexpectedly")},
	} {
		calls = 0
		run := func(ctx context.Context, args ...string) (string, error) {
			calls++
			if calls <= len(errs) {
				return "", errs[calls-1]
			}
			return "", nil
		}
		if err := newSession(context.Background(), run, "s", "/tmp", "sh"); err == nil {
			t.Errorf("errors %v: newSession succeeded after %d calls", errs, calls)
		}
	}
}
