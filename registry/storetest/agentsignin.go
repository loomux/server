package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// runAgentSignIn is the conformance suite for an agent's sign-in state
// on a target (LOOM-316).
func runAgentSignIn(t *testing.T, newStore func(t *testing.T) registry.Store) {
	t.Run("TargetAgentAuthDetails", func(t *testing.T) { testTargetAgentAuthDetails(t, newStore(t)) })
	t.Run("GetTargetAgent", func(t *testing.T) { testGetTargetAgent(t, newStore(t)) })
	t.Run("AgentTurnIssues", func(t *testing.T) { testAgentTurnIssues(t, newStore(t)) })
	t.Run("AgentSignInSession", func(t *testing.T) { testAgentSignInSession(t, newStore(t)) })
	t.Run("AgentTurnStateNeedsARow", func(t *testing.T) { testAgentTurnStateNeedsARow(t, newStore(t)) })
}

func setProbed(t *testing.T, s registry.Store, a *registry.TargetAgent) {
	t.Helper()
	if err := s.SetTargetAgent(context.Background(), a); err != nil {
		t.Fatalf("SetTargetAgent: %v", err)
	}
}

func getAgent(t *testing.T, s registry.Store, targetID, agentType string) *registry.TargetAgent {
	t.Helper()
	got, err := s.GetTargetAgent(context.Background(), targetID, agentType)
	if err != nil {
		t.Fatalf("GetTargetAgent: %v", err)
	}
	return got
}

// The probe's details round-trip, and a later probe replaces them.
func testTargetAgentAuthDetails(t *testing.T, s registry.Store) {
	target := createTestTarget(t, s)
	expires := time.Date(2026, 12, 11, 1, 4, 18, 0, time.UTC)
	setProbed(t, s, &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code", Available: true,
		Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedIn,
		AuthMethod: "claude.ai", AuthPlan: "max", AuthExpiresAt: &expires})
	got := getAgent(t, s, target.ID, "claude-code")
	if got.AuthMethod != "claude.ai" || got.AuthPlan != "max" || got.AuthExpiresAt == nil || !got.AuthExpiresAt.Equal(expires) {
		t.Fatalf("details = %q %q %v, want claude.ai max %v", got.AuthMethod, got.AuthPlan, got.AuthExpiresAt, expires)
	}
	list, err := s.ListTargetAgents(context.Background(), target.ID)
	if err != nil || len(list) != 1 || list[0].AuthPlan != "max" || list[0].AuthExpiresAt == nil {
		t.Fatalf("ListTargetAgents = %+v, %v; want the details too", list, err)
	}

	setProbed(t, s, &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code", Available: true,
		Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedOut, AuthMethod: "none"})
	got = getAgent(t, s, target.ID, "claude-code")
	if got.AuthMethod != "none" || got.AuthPlan != "" || got.AuthExpiresAt != nil {
		t.Fatalf("after a signed-out probe, details = %q %q %v, want none, empty, nil", got.AuthMethod, got.AuthPlan, got.AuthExpiresAt)
	}
}

func testGetTargetAgent(t *testing.T, s registry.Store) {
	target := createTestTarget(t, s)
	if _, err := s.GetTargetAgent(context.Background(), target.ID, "codex"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetTargetAgent (never probed) err = %v, want ErrNotFound", err)
	}
	setProbed(t, s, &registry.TargetAgent{TargetID: target.ID, AgentType: "codex", Available: true, Path: "/usr/bin/codex"})
	if got := getAgent(t, s, target.ID, "codex"); got.Path != "/usr/bin/codex" || !got.Available {
		t.Fatalf("GetTargetAgent = %+v", got)
	}
}

// What turns saw is set and cleared on its own, and a probe leaves it
// alone.
func testAgentTurnIssues(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	probe := &registry.TargetAgent{TargetID: target.ID, AgentType: "claude-code", Available: true,
		Path: "/usr/bin/claude", AuthStatus: registry.AgentAuthLoggedIn}
	setProbed(t, s, probe)

	seen := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if err := s.SetAgentSignInProblem(ctx, target.ID, "claude-code", registry.AgentSignInExpired, seen); err != nil {
		t.Fatalf("SetAgentSignInProblem: %v", err)
	}
	limited := seen.Add(time.Minute)
	if err := s.SetAgentRateLimit(ctx, target.ID, "claude-code", limited, "5pm (Europe/Berlin)"); err != nil {
		t.Fatalf("SetAgentRateLimit: %v", err)
	}
	setProbed(t, s, probe) // a later probe
	got := getAgent(t, s, target.ID, "claude-code")
	if got.SignInProblem != registry.AgentSignInExpired || got.SignInProblemAt == nil || !got.SignInProblemAt.Equal(seen) {
		t.Errorf("sign-in problem = %q at %v, want expired at %v", got.SignInProblem, got.SignInProblemAt, seen)
	}
	if got.LimitedAt == nil || !got.LimitedAt.Equal(limited) || got.LimitResets != "5pm (Europe/Berlin)" {
		t.Errorf("rate limit = %v %q, want %v and its reset", got.LimitedAt, got.LimitResets, limited)
	}

	if err := s.SetAgentSignInProblem(ctx, target.ID, "claude-code", "", time.Time{}); err != nil {
		t.Fatalf("SetAgentSignInProblem (clear): %v", err)
	}
	got = getAgent(t, s, target.ID, "claude-code")
	if got.SignInProblem != "" || got.SignInProblemAt != nil || got.LimitedAt == nil {
		t.Errorf("after clearing the problem: problem %q at %v, limit %v; want only the limit left", got.SignInProblem, got.SignInProblemAt, got.LimitedAt)
	}

	if err := s.SetAgentSignInProblem(ctx, target.ID, "claude-code", registry.AgentSignInSignedOut, seen); err != nil {
		t.Fatalf("SetAgentSignInProblem: %v", err)
	}
	if err := s.ClearAgentTurnIssues(ctx, target.ID, "claude-code"); err != nil {
		t.Fatalf("ClearAgentTurnIssues: %v", err)
	}
	got = getAgent(t, s, target.ID, "claude-code")
	if got.SignInProblem != "" || got.SignInProblemAt != nil || got.LimitedAt != nil || got.LimitResets != "" {
		t.Errorf("after ClearAgentTurnIssues: %+v, want no problem and no limit", got)
	}
	// Nothing to clear is not an error.
	if err := s.ClearAgentTurnIssues(ctx, target.ID, "claude-code"); err != nil {
		t.Errorf("ClearAgentTurnIssues (nothing set): %v", err)
	}
	if err := s.SetAgentRateLimit(ctx, target.ID, "claude-code", time.Time{}, "ignored"); err != nil {
		t.Fatalf("SetAgentRateLimit (clear): %v", err)
	}
	if got = getAgent(t, s, target.ID, "claude-code"); got.LimitedAt != nil || got.LimitResets != "" {
		t.Errorf("a zero time clears the limit: got %v %q", got.LimitedAt, got.LimitResets)
	}
}

func testAgentSignInSession(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	probe := &registry.TargetAgent{TargetID: target.ID, AgentType: "codex", Available: true,
		Path: "/usr/bin/codex", AuthStatus: registry.AgentAuthLoggedOut}
	setProbed(t, s, probe)
	started := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	if err := s.SetAgentSignInSession(ctx, target.ID, "codex", "loomux-signin-codex", started); err != nil {
		t.Fatalf("SetAgentSignInSession: %v", err)
	}
	setProbed(t, s, probe) // a later probe keeps it
	got := getAgent(t, s, target.ID, "codex")
	if got.SignInSession != "loomux-signin-codex" || got.SignInStartedAt == nil || !got.SignInStartedAt.Equal(started) {
		t.Fatalf("sign-in session = %q since %v, want loomux-signin-codex since %v", got.SignInSession, got.SignInStartedAt, started)
	}
	if err := s.SetAgentSignInSession(ctx, target.ID, "codex", "", time.Time{}); err != nil {
		t.Fatalf("SetAgentSignInSession (clear): %v", err)
	}
	if got = getAgent(t, s, target.ID, "codex"); got.SignInSession != "" || got.SignInStartedAt != nil {
		t.Fatalf("cleared sign-in session = %q since %v", got.SignInSession, got.SignInStartedAt)
	}
}

// The turn and sign-in writers update a probed row; with none there is
// nothing to update.
func testAgentTurnStateNeedsARow(t *testing.T, s registry.Store) {
	ctx := context.Background()
	target := createTestTarget(t, s)
	now := time.Now().UTC()
	for name, err := range map[string]error{
		"SetAgentSignInProblem": s.SetAgentSignInProblem(ctx, target.ID, "codex", registry.AgentSignInSignedOut, now),
		"SetAgentRateLimit":     s.SetAgentRateLimit(ctx, target.ID, "codex", now, ""),
		"SetAgentSignInSession": s.SetAgentSignInSession(ctx, target.ID, "codex", "loomux-signin-codex", now),
	} {
		if !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s with no probed row: err = %v, want ErrNotFound", name, err)
		}
	}
}
