package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/router"
)

// PrepareAgentSignIn opens agentType's sign-in on targetID for a person
// to attach to (LOOM-316) — see router.Router.PrepareSignIn. Satisfies
// api.AgentSignIns.
func (a *App) PrepareAgentSignIn(ctx context.Context, targetID, agentType string, restart bool) (*api.AgentSignInResult, error) {
	res, err := a.router.PrepareSignIn(ctx, targetID, agentType, restart)
	if err != nil {
		return nil, signInErrorForAPI(err)
	}
	return &api.AgentSignInResult{Agent: res.Agent, Opened: res.Opened, Reused: res.Reused}, nil
}

// CloseAgentSignIn closes agentType's sign-in session on targetID
// (LOOM-316). Satisfies api.AgentSignIns.
func (a *App) CloseAgentSignIn(ctx context.Context, targetID, agentType string) error {
	return signInErrorForAPI(a.router.CloseSignIn(ctx, targetID, agentType))
}

// signInErrorForAPI makes the router's sign-in errors the API's; others
// (registry.ErrNotFound, targets.ErrUnreachable) pass through.
func signInErrorForAPI(err error) error {
	var unavailable *router.AgentUnavailableError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, router.ErrUnknownAgentType):
		return fmt.Errorf("%w: %v", api.ErrUnknownAgentType, err)
	case errors.Is(err, router.ErrNoSignIn):
		return fmt.Errorf("%w: %v", api.ErrNoAgentSignIn, err)
	case errors.As(err, &unavailable):
		return fmt.Errorf("%w: %v", api.ErrAgentNotInstalled, err)
	}
	return err
}
