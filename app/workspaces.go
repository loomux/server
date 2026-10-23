package app

import (
	"context"
	"errors"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/router"
)

// ProvisionWorkspace provisions a workspace on a chosen target for POST
// /api/v1/workspaces (LOOM-215): the router writes the row and the run
// goes on in the background, on the app's own context, so Close waits
// for it (and, cancelling it, leaves the workspace failed with why).
// Satisfies api.WorkspaceProvisioner.
func (a *App) ProvisionWorkspace(ctx context.Context, req api.WorkspaceToProvision) (api.ProvisionedWorkspace, error) {
	p, err := a.router.ProvisionWorkspace(ctx, router.ProvisionSpec{
		TargetID: req.TargetID, Name: req.Name, Kind: router.ProvisionKind(req.Kind), GitRemote: req.GitRemote,
		Description: req.Description, Tags: req.Tags,
	})
	var (
		specErr *router.SpecError
		taken   *router.NameTakenError
	)
	switch {
	case errors.Is(err, router.ErrNoSuchTarget):
		return api.ProvisionedWorkspace{}, api.ErrNoSuchTarget
	case errors.As(err, &specErr):
		return api.ProvisionedWorkspace{}, &api.WorkspaceSpecError{Msg: specErr.Error()}
	case errors.As(err, &taken):
		return api.ProvisionedWorkspace{}, &api.WorkspaceNameTakenError{Msg: taken.Error()}
	case err != nil:
		return api.ProvisionedWorkspace{}, err
	}
	if p.Run != nil {
		run := p.Run
		if !a.bg.Go(func() { run(a.bgCtx) }) {
			// Closing: nothing will run it. The reaper (or the next
			// start's) fails the row once it is stale.
			return api.ProvisionedWorkspace{}, errors.New("app: shutting down")
		}
	}
	return api.ProvisionedWorkspace{Workspace: p.Workspace, ConversationID: p.ConversationID, Existing: p.Existing}, nil
}
