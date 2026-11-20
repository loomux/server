package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// LOOM-316: the router's sign-in errors reach the API as the errors it
// maps to statuses; anything else passes through unchanged.
func TestSignInErrorForAPI(t *testing.T) {
	unavailable := &router.AgentUnavailableError{AgentType: "codex", TargetID: "t1", TargetName: "jet01"}
	cases := []struct {
		in   error
		want error
	}{
		{fmt.Errorf("%w %q", router.ErrUnknownAgentType, "nope"), api.ErrUnknownAgentType},
		{fmt.Errorf("%w: %q", router.ErrNoSignIn, "aider"), api.ErrNoAgentSignIn},
		{unavailable, api.ErrAgentNotInstalled},
		{fmt.Errorf("router: prepare sign-in: %w", registry.ErrNotFound), registry.ErrNotFound},
		{fmt.Errorf("router: prepare sign-in: %w", targets.ErrUnreachable), targets.ErrUnreachable},
	}
	for _, tc := range cases {
		if got := signInErrorForAPI(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("signInErrorForAPI(%v) = %v, want it to be %v", tc.in, got, tc.want)
		}
	}
	if signInErrorForAPI(nil) != nil {
		t.Error("nil became an error")
	}
}

// A sign-in session is reported only as long as the orphan sweep leaves
// it alone.
func TestSignInSessionTTLIsTheOrphanSweeps(t *testing.T) {
	if registry.SignInSessionTTL != orphanTTL {
		t.Errorf("registry.SignInSessionTTL = %s, orphanTTL = %s; keep them equal", registry.SignInSessionTTL, orphanTTL)
	}
}
