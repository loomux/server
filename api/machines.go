package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Machines backs the machine side of /api/v1/targets (LOOM-178,
// docs/design/target-providers.md §5): targets a plugin made. Satisfied
// by *providers.Manager.
type Machines interface {
	View(ctx context.Context, targetID string) (*providers.MachineView, error)
	Views(ctx context.Context) (map[string]*providers.MachineView, error)
	IsMachine(ctx context.Context, targetID string) bool
	Create(ctx context.Context, req providers.CreateRequest) (*registry.Target, *registry.Environment, error)
	Start(ctx context.Context, targetID string) error
	Stop(ctx context.Context, targetID string) error
	Recreate(ctx context.Context, targetID, size string) error
	DeleteTarget(ctx context.Context, targetID string) error
	AttachCommands(ctx context.Context, targetID, session string) ([]protocol.AttachCommand, error)
}

// WithMachines enables machines on /api/v1/targets (LOOM-178).
func WithMachines(m Machines) Option {
	return func(s *Server) { s.machines = m }
}

// targetPluginRequest is POST /targets' plugin object: make the target
// as a machine of that plugin instance.
type targetPluginRequest struct {
	ID         string `json:"id"`
	Size       string `json:"size"`
	Persistent *bool  `json:"persistent"`
	Egress     string `json:"egress"`
}

// targetPluginResponse is a target's plugin object: null for a
// registered host.
type targetPluginResponse struct {
	// ID, Name and Label are the plugin instance that owns the machine;
	// ID is empty once it was uninstalled keeping its machines.
	ID            string `json:"id"`
	Name          string `json:"name"`
	Label         string `json:"label"`
	Version       string `json:"version"`
	EnvironmentID string `json:"environment_id"`
	// Status is creating, starting, running, stopped, recreating,
	// destroying, lost, error or detached; StatusReason says why for
	// error, lost and detached.
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	Size         string `json:"size"`
	Persistent   bool   `json:"persistent"`
	Egress       string `json:"egress"`
	Image        string `json:"image"`
	ImageDigest  string `json:"image_digest"`
	// UpdateAvailable: the plugin's configured image differs from the
	// one the machine runs; recreate applies it.
	UpdateAvailable bool `json:"update_available"`
	// Warnings are the plugin's current warnings about every machine it
	// makes (network isolation not enforced, say).
	Warnings  []warningResponse `json:"warnings"`
	CreatedAt time.Time         `json:"created_at"`
}

type warningResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// recreateTargetRequest: size is optional (the current one stays).
type recreateTargetRequest struct {
	Size string `json:"size"`
}

// attachCommandResponse is one way a person attaches to a machine's
// tmux (via kubectl, docker, ssh…).
type attachCommandResponse struct {
	Via     string `json:"via"`
	Command string `json:"command"`
}

func newTargetPluginResponse(v *providers.MachineView) *targetPluginResponse {
	e := v.Environment
	out := &targetPluginResponse{
		ID: e.PluginID, Name: e.PluginName, Label: v.PluginLabel, Version: e.PluginVersion, EnvironmentID: e.ID,
		Status: string(e.Status), StatusReason: e.StatusReason, Size: e.Size, Persistent: e.Persistent, Egress: e.Egress,
		Image: e.Image, ImageDigest: e.ImageDigest, UpdateAvailable: v.UpdateAvailable, Warnings: []warningResponse{}, CreatedAt: e.CreatedAt,
	}
	for _, w := range v.Warnings {
		out.Warnings = append(out.Warnings, warningResponse{Code: w.Code, Message: w.Message})
	}
	return out
}

// decorateMachine fills resp.Plugin for a machine; a registered host
// keeps null.
func (s *Server) decorateMachine(ctx context.Context, resp *targetResponse, targetID string) {
	if s.machines == nil {
		return
	}
	v, err := s.machines.View(ctx, targetID)
	if err != nil {
		return
	}
	resp.Plugin = newTargetPluginResponse(v)
}

// decorateMachines fills Plugin on a list of targets in one pass.
func (s *Server) decorateMachines(ctx context.Context, resps []targetResponse) {
	if s.machines == nil {
		return
	}
	views, err := s.machines.Views(ctx)
	if err != nil {
		return
	}
	for i := range resps {
		if v := views[resps[i].ID]; v != nil {
			resps[i].Plugin = newTargetPluginResponse(v)
		}
	}
}

// refuseIfMachine answers 409 for a route that doesn't apply to a
// machine (its connection is the plugin's to manage) and reports whether
// it did.
func (s *Server) refuseIfMachine(w http.ResponseWriter, r *http.Request) bool {
	if s.machines == nil {
		return false
	}
	if s.machines.IsMachine(r.Context(), r.PathValue("id")) {
		writeError(w, http.StatusConflict, "this target is a machine a plugin made; its connection is the plugin's to manage")
		return true
	}
	return false
}

// handleGetTarget: GET /api/v1/targets/{id}.
func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target, err := s.targets.GetTarget(r.Context(), id)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch target")
		return
	}
	writeJSON(w, http.StatusOK, s.targetView(r.Context(), target))
}

// createMachine is POST /targets with a plugin object.
func (s *Server) createMachine(w http.ResponseWriter, r *http.Request, target *registry.Target, req *targetPluginRequest) {
	if s.machines == nil {
		writeError(w, http.StatusNotFound, "machines are not available on this server")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "plugin.id is required")
		return
	}
	created, _, err := s.machines.Create(r.Context(), providers.CreateRequest{
		Target: *target, PluginID: req.ID, Size: req.Size, Persistent: req.Persistent, Egress: req.Egress,
	})
	if !s.writeMachineError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, s.targetView(r.Context(), created))
}

func (s *Server) handleStartTarget(w http.ResponseWriter, r *http.Request) {
	s.machineAction(w, r, func(ctx context.Context, id string) error { return s.machines.Start(ctx, id) })
}

func (s *Server) handleStopTarget(w http.ResponseWriter, r *http.Request) {
	s.machineAction(w, r, func(ctx context.Context, id string) error { return s.machines.Stop(ctx, id) })
}

func (s *Server) handleRecreateTarget(w http.ResponseWriter, r *http.Request) {
	var req recreateTargetRequest
	if r.ContentLength != 0 {
		if !readJSON(w, r, &req) {
			return
		}
	}
	s.machineAction(w, r, func(ctx context.Context, id string) error { return s.machines.Recreate(ctx, id, req.Size) })
}

// machineAction runs a lifecycle verb and answers 202 with the target.
func (s *Server) machineAction(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, id string) error) {
	if s.machines == nil {
		writeError(w, http.StatusNotFound, "machines are not available on this server")
		return
	}
	id := r.PathValue("id")
	target, err := s.targets.GetTarget(r.Context(), id)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch target")
		return
	}
	if !s.writeMachineError(w, action(r.Context(), id)) {
		return
	}
	writeJSON(w, http.StatusAccepted, s.targetView(r.Context(), target))
}

// deleteMachine is DELETE /targets/{id} for a machine: the cascade.
func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, id string) {
	if !s.writeMachineError(w, s.machines.DeleteTarget(r.Context(), id)) {
		return
	}
	s.forgetScan(id)
	w.WriteHeader(http.StatusNoContent)
}

// writeMachineError maps a providers error to a response and reports
// whether the caller may go on.
func (s *Server) writeMachineError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	var (
		invalid  *providers.InvalidError
		badState *providers.BadStateError
		badAddr  *providers.BadAddressError
		missing  *plugins.CapabilityMissingError
	)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such target")
	case errors.Is(err, providers.ErrNotMachine):
		writeError(w, http.StatusConflict, "this target is a registered host, not a machine a plugin made")
	case errors.Is(err, providers.ErrPluginUnavailable):
		writeError(w, http.StatusConflict, "the plugin that owns this machine isn't installed, enabled and running")
	case errors.Is(err, providers.ErrDetached):
		writeError(w, http.StatusConflict, "the plugin that made this machine was uninstalled; nothing manages it any more")
	case errors.Is(err, providers.ErrTaskActive):
		writeError(w, http.StatusConflict, "a task is running on this machine; cancel it or let it finish first")
	case errors.Is(err, providers.ErrEphemeralStop):
		writeError(w, http.StatusConflict, "an ephemeral machine keeps nothing when stopped; delete it instead")
	case errors.Is(err, providers.ErrQuota):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Field+": "+invalid.Msg)
	case errors.As(err, &badState):
		writeError(w, http.StatusConflict, badState.Error())
	case errors.As(err, &badAddr):
		writeError(w, http.StatusBadGateway, badAddr.Error())
	case errors.As(err, &missing):
		writeError(w, http.StatusConflict, "the plugin doesn't support this: it doesn't declare "+missing.Capability)
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, registry.ErrNoMasterKey):
		writeError(w, http.StatusServiceUnavailable, "machines need LOOMUX_MASTER_KEY, which this server doesn't have")
	default:
		writeError(w, http.StatusInternalServerError, "machine operation failed")
	}
	return false
}

// attachCommandsFor is attach-info's attach_commands: the plugin's ways
// in for a machine, and the plain ssh form for every remote target.
func (s *Server) attachCommandsFor(ctx context.Context, target *registry.Target, session string) []attachCommandResponse {
	out := []attachCommandResponse{}
	if s.machines != nil {
		if cmds, err := s.machines.AttachCommands(ctx, target.ID, session); err == nil {
			for _, c := range cmds {
				out = append(out, attachCommandResponse{Via: c.Via, Command: c.Command})
			}
		}
	}
	if target.Kind == registry.TargetKindRemote {
		out = append(out, attachCommandResponse{Via: "ssh", Command: sshAttachCommand(target, session)})
	}
	return out
}

// targetView is one target as the API shows it, with everything a list
// entry carries (health, the managed key, onboarding state, the plugin).
func (s *Server) targetView(ctx context.Context, t *registry.Target) targetResponse {
	views, err := s.targetResponses(ctx, []*registry.Target{t})
	if err != nil || len(views) != 1 {
		resp := newTargetResponse(t)
		s.decorateMachine(ctx, &resp, t.ID)
		return resp
	}
	return views[0]
}

// sshAttachCommand is the plain ssh form of attaching to session on a
// remote target.
func sshAttachCommand(t *registry.Target, session string) string {
	cmd := "ssh "
	if t.SSHPort != 0 && t.SSHPort != 22 {
		cmd += fmt.Sprintf("-p %d ", t.SSHPort)
	}
	return cmd + t.User + "@" + t.Host + " -t " + shellQuote(targets.AttachCommand(session))
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
