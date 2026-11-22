package registry_test

import (
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// LOOM-316: the one word a client shows for an agent's sign-in, in
// order: the CLI saying it's signed out, then what a turn saw, then the
// sign-in's own end, then a usage limit, then what the CLI said.
func TestTargetAgentSignInState(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(24*time.Hour)
	cases := []struct {
		name string
		a    registry.TargetAgent
		want string
	}{
		{"not checked", registry.TargetAgent{Available: true}, ""},
		{"not installed", registry.TargetAgent{AuthStatus: registry.AgentAuthLoggedOut}, ""},
		{"signed in", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn, AuthExpiresAt: &future},
			registry.AgentAuthLoggedIn},
		{"unknown", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthUnknown}, registry.AgentAuthUnknown},
		{"signed out wins", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedOut,
			SignInProblem: registry.AgentSignInExpired, SignInProblemAt: &past, LimitedAt: &past}, registry.AgentAuthLoggedOut},
		{"a turn saw it expired", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn,
			SignInProblem: registry.AgentSignInExpired, SignInProblemAt: &past}, registry.AgentAuthExpired},
		{"a turn saw a bad key", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn,
			SignInProblem: registry.AgentSignInInvalid, SignInProblemAt: &past}, registry.AgentAuthExpired},
		{"a turn saw the login screen", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn,
			SignInProblem: registry.AgentSignInSignedOut, SignInProblemAt: &past}, registry.AgentAuthLoggedOut},
		{"its sign-in ended", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn, AuthExpiresAt: &past},
			registry.AgentAuthExpired},
		{"rate limited", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn, LimitedAt: &past},
			registry.AgentAuthRateLimited},
		{"expired over rate limited", registry.TargetAgent{Available: true, AuthStatus: registry.AgentAuthLoggedIn,
			AuthExpiresAt: &past, LimitedAt: &past}, registry.AgentAuthExpired},
	}
	for _, tc := range cases {
		if got := tc.a.SignInState(now); got != tc.want {
			t.Errorf("%s: SignInState = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A sign-in session is reported only while the orphan sweep would still
// leave it alone.
func TestTargetAgentOpenSignInSession(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-registry.SignInSessionTTL-time.Minute)
	if got := (&registry.TargetAgent{SignInSession: "loomux-signin-codex", SignInStartedAt: &recent}).OpenSignInSession(now); got != "loomux-signin-codex" {
		t.Errorf("recent session = %q, want it", got)
	}
	if got := (&registry.TargetAgent{SignInSession: "loomux-signin-codex", SignInStartedAt: &old}).OpenSignInSession(now); got != "" {
		t.Errorf("session past the TTL = %q, want none", got)
	}
	if got := (&registry.TargetAgent{}).OpenSignInSession(now); got != "" {
		t.Errorf("no session = %q", got)
	}
}
