package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Loomux/server/registry"
)

// Provisioning a workspace through the API (LOOM-215): POST
// /api/v1/workspaces does what a chat's provision_workspace does, on a
// target the caller names, and answers once the workspace row exists
// (202, status provisioning); GET /api/v1/workspaces/{id} follows it to
// idle or failed (with status_reason). The provisioning run itself is
// recorded under a conversation of its own, named in the 202, where its
// command, output and events can be read like any turn's.

// WorkspaceProvisioner provisions a workspace on a chosen target:
// app.App, over the router.
type WorkspaceProvisioner interface {
	// ProvisionWorkspace writes the workspace row and starts its
	// provisioning in the background. The same name on the same target
	// from the same source returns the workspace already there
	// (Existing). Errors a caller can act on: ErrNoSuchTarget,
	// *WorkspaceSpecError, *WorkspaceNameTakenError.
	ProvisionWorkspace(ctx context.Context, req WorkspaceToProvision) (ProvisionedWorkspace, error)
}

// WorkspaceToProvision is a workspace to make: Kind is empty,
// git_clone, existing_dir or git_worktree (LOOM-228: Worktree's base,
// ref and branch, only then).
type WorkspaceToProvision struct {
	TargetID, Name, Kind, GitRemote, Description string
	Tags                                         []string
	Worktree                                     WorktreeToAdd
}

// WorktreeToAdd is a git_worktree workspace's source: the repository's
// directory under the workspace root, the ref the branch starts at
// ("" for its HEAD) and the new branch ("" for loomux/<name>).
type WorktreeToAdd struct {
	Base, Ref, Branch string
}

// ProvisionedWorkspace is what ProvisionWorkspace did.
type ProvisionedWorkspace struct {
	Workspace *registry.Workspace
	// ConversationID is the conversation the provisioning run is
	// recorded under; "" for an Existing workspace.
	ConversationID string
	Existing       bool
}

// ErrNoSuchTarget: the target_id names no target.
var ErrNoSuchTarget = errors.New("no such target")

// WorkspaceSpecError is a request to correct; Msg says how.
type WorkspaceSpecError struct{ Msg string }

func (e *WorkspaceSpecError) Error() string { return e.Msg }

// WorkspaceNameTakenError: another workspace has the name; Msg says
// which and what to do.
type WorkspaceNameTakenError struct{ Msg string }

func (e *WorkspaceNameTakenError) Error() string { return e.Msg }

// codeWorkspaceNameTaken is POST /workspaces' 409 when the name is
// another workspace's.
const codeWorkspaceNameTaken = "workspace_name_taken"

// WithWorkspaceProvisioner enables POST /api/v1/workspaces (LOOM-215).
func WithWorkspaceProvisioner(p WorkspaceProvisioner) Option {
	return func(s *Server) { s.workspaceProvisioner = p }
}

type createWorkspaceRequest struct {
	TargetID    string   `json:"target_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	GitRemote   string   `json:"git_remote"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	// Worktree makes the workspace a git worktree of a repository
	// already on the target (LOOM-228); kind is git_worktree with it.
	Worktree *worktreeRequest `json:"worktree"`
}

type worktreeRequest struct {
	Base   string `json:"base"`
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
}

// workspaceCreatedResponse is a workspace as the list shows it, plus
// the conversation its provisioning is recorded under.
type workspaceCreatedResponse struct {
	workspaceSummary
	ConversationID string `json:"conversation_id,omitempty"`
}

// handleCreateWorkspace: POST /api/v1/workspaces. 202 with the new
// workspace (status provisioning) and a Location to follow; 200 with the
// workspace already there for a repeat of the same request; 400 for a
// request to correct (an unknown target_id included); 409
// workspace_name_taken when the name is another workspace's.
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.workspaceProvisioner == nil {
		writeError(w, http.StatusNotImplemented, "workspace provisioning is not configured on this server")
		return
	}
	var req createWorkspaceRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.TargetID, req.Name, req.GitRemote = strings.TrimSpace(req.TargetID), strings.TrimSpace(req.Name), strings.TrimSpace(req.GitRemote)
	switch {
	case req.TargetID == "":
		writeError(w, http.StatusBadRequest, "target_id is required")
		return
	case req.Name == "":
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	var worktree WorktreeToAdd
	if req.Worktree != nil {
		worktree = WorktreeToAdd{Base: strings.TrimSpace(req.Worktree.Base), Ref: strings.TrimSpace(req.Worktree.Ref), Branch: strings.TrimSpace(req.Worktree.Branch)}
	}
	if req.Kind == "" {
		switch {
		case req.Worktree != nil:
			req.Kind = "git_worktree"
		case req.GitRemote != "":
			req.Kind = "git_clone"
		default:
			req.Kind = "empty"
		}
	}
	if req.Kind == "git_worktree" && req.Worktree == nil {
		writeError(w, http.StatusBadRequest, "kind git_worktree needs worktree: {base, ref, branch}")
		return
	}
	got, err := s.workspaceProvisioner.ProvisionWorkspace(r.Context(), WorkspaceToProvision{
		TargetID: req.TargetID, Name: req.Name, Kind: req.Kind, GitRemote: req.GitRemote,
		Description: req.Description, Tags: req.Tags, Worktree: worktree,
	})
	var (
		specErr *WorkspaceSpecError
		taken   *WorkspaceNameTakenError
	)
	switch {
	case errors.Is(err, ErrNoSuchTarget):
		writeError(w, http.StatusBadRequest, "no such target")
		return
	case errors.As(err, &specErr):
		writeError(w, http.StatusBadRequest, specErr.Msg)
		return
	case errors.As(err, &taken):
		writeErrorCode(w, http.StatusConflict, codeWorkspaceNameTaken, taken.Msg)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not provision the workspace")
		return
	}
	out := workspaceCreatedResponse{workspaceSummary: newWorkspaceSummary(got.Workspace), ConversationID: got.ConversationID}
	if got.Existing {
		writeJSON(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Location", "/api/v1/workspaces/"+got.Workspace.ID)
	writeJSON(w, http.StatusAccepted, out)
}

// handleGetWorkspace: GET /api/v1/workspaces/{id}, one workspace as the
// list shows it: what a client polls while one provisions (LOOM-215).
func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.workspaces.GetWorkspace(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read the workspace")
		return
	}
	writeJSON(w, http.StatusOK, newWorkspaceSummary(ws))
}

// newWorkspaceSummary is ws as the API shows it.
func newWorkspaceSummary(ws *registry.Workspace) workspaceSummary {
	return workspaceSummary{
		ID:             ws.ID,
		Name:           ws.Name,
		TargetID:       ws.TargetID,
		Status:         string(ws.Status),
		Tags:           orEmpty(ws.Tags),
		Description:    ws.Description,
		RollingSummary: ws.RollingSummary,
		LastUsedAt:     ws.LastUsedAt,
		StatusReason:   ws.StatusReason,
	}
}
