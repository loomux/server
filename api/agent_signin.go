package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// AgentSignIns opens and closes agent sign-in sessions (LOOM-316): a tmux
// session on a target running the agent CLI's own login, for a person to
// attach to and finish. Loomux never signs an agent in itself.
type AgentSignIns interface {
	// PrepareAgentSignIn probes agentType on targetID and, unless it is
	// signed in (or restart), opens its sign-in session or hands back the
	// one already open. Errors: ErrUnknownAgentType, ErrNoAgentSignIn,
	// ErrAgentNotInstalled, registry.ErrNotFound (no such target),
	// targets.ErrUnreachable.
	PrepareAgentSignIn(ctx context.Context, targetID, agentType string, restart bool) (*AgentSignInResult, error)
	// CloseAgentSignIn closes agentType's sign-in session on targetID if
	// one is open.
	CloseAgentSignIn(ctx context.Context, targetID, agentType string) error
}

// AgentSignInResult is what PrepareAgentSignIn did.
type AgentSignInResult struct {
	// Agent is the agent's record after its probe, its sign-in session
	// included.
	Agent *registry.TargetAgent
	// Opened: a sign-in session was opened now. Reused: the one already
	// open was handed back. Neither: the agent is signed in.
	Opened bool
	Reused bool
}

var (
	// ErrUnknownAgentType: no agent type of that name.
	ErrUnknownAgentType = errors.New("api: unknown agent type")
	// ErrNoAgentSignIn: the agent type has no sign-in command.
	ErrNoAgentSignIn = errors.New("api: agent type has no sign-in")
	// ErrAgentNotInstalled: the agent's CLI isn't installed on the
	// target.
	ErrAgentNotInstalled = errors.New("api: agent not installed on the target")
)

type agentSignInRequest struct {
	// Restart replaces a sign-in session already open, and opens one even
	// for a signed-in agent (to sign in to another account).
	Restart bool `json:"restart"`
}

type agentSignInResponse struct {
	Agent  targetAgentResponse `json:"agent"`
	Opened bool                `json:"opened"`
	Reused bool                `json:"reused"`
}

// handleAgentSignIn opens an agent's sign-in on a target (LOOM-316): the
// response is the agent with its sign-in state and, when a session is
// open, the commands to attach to it. An empty body is restart false.
func (s *Server) handleAgentSignIn(w http.ResponseWriter, r *http.Request) {
	if s.agentSignIns == nil {
		writeError(w, http.StatusNotImplemented, "agent sign-in is not configured on this server")
		return
	}
	var req agentSignInRequest
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	id, agentType := r.PathValue("id"), r.PathValue("agent_type")
	res, err := s.agentSignIns.PrepareAgentSignIn(r.Context(), id, agentType, req.Restart)
	if err != nil {
		writeAgentSignInError(w, err, "could not open the agent's sign-in")
		return
	}
	target, err := s.targets.GetTarget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read target")
		return
	}
	writeJSON(w, http.StatusOK, agentSignInResponse{
		Agent:  s.targetAgentView(r.Context(), target, res.Agent, time.Now()),
		Opened: res.Opened, Reused: res.Reused,
	})
}

// handleCloseAgentSignIn closes an agent's sign-in session on a target
// (LOOM-316); none open is fine.
func (s *Server) handleCloseAgentSignIn(w http.ResponseWriter, r *http.Request) {
	if s.agentSignIns == nil {
		writeError(w, http.StatusNotImplemented, "agent sign-in is not configured on this server")
		return
	}
	if err := s.agentSignIns.CloseAgentSignIn(r.Context(), r.PathValue("id"), r.PathValue("agent_type")); err != nil {
		writeAgentSignInError(w, err, "could not close the agent's sign-in")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAgentSignInError maps a sign-in error to its response; anything
// unexpected is a 500 saying only internal.
func writeAgentSignInError(w http.ResponseWriter, err error, internal string) {
	switch {
	case errors.Is(err, ErrUnknownAgentType):
		writeError(w, http.StatusNotFound, "no such agent type")
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such target")
	case errors.Is(err, ErrNoAgentSignIn):
		writeErrorCode(w, http.StatusUnprocessableEntity, "no_sign_in", "this agent type has no sign-in to open")
	case errors.Is(err, ErrAgentNotInstalled):
		writeErrorCode(w, http.StatusConflict, "agent_unavailable", "the agent's CLI isn't installed on this target")
	case errors.Is(err, targets.ErrUnreachable):
		writeErrorCode(w, http.StatusBadGateway, "target_unreachable", "the target couldn't be reached")
	default:
		writeError(w, http.StatusInternalServerError, internal)
	}
}
