package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
)

// Plugins backs /api/v1/plugins (LOOM-178, docs/design/target-providers.md
// §1.5, §5): the catalog of available plugins and the installed ones'
// lifecycle. Satisfied by *plugins.Manager. Secrets go in and never come
// out: every view carries them as set or not set.
type Plugins interface {
	Available(ctx context.Context) ([]plugins.Available, error)
	Install(ctx context.Context, r plugins.InstallRequest) (*plugins.View, error)
	Get(ctx context.Context, id string) (*plugins.View, error)
	List(ctx context.Context) ([]*plugins.View, error)
	SetConfig(ctx context.Context, id string, config map[string]any) (*plugins.View, error)
	Enable(ctx context.Context, id string) (*plugins.View, error)
	Disable(ctx context.Context, id string) (*plugins.View, error)
	Upgrade(ctx context.Context, id string) (*plugins.View, error)
	Check(ctx context.Context, id string) (*plugins.View, error)
	Uninstall(ctx context.Context, id, targets string) error
}

// WithPlugins enables /api/v1/plugins (LOOM-178).
func WithPlugins(p Plugins) Option {
	return func(s *Server) { s.plugins = p }
}

// availablePluginResponse is one catalog entry.
type availablePluginResponse struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	Version        string `json:"version"`
	Protocol       string `json:"protocol"`
	Vendor         string `json:"vendor"`
	Homepage       string `json:"homepage"`
	Description    string `json:"description"`
	MinHostVersion string `json:"min_host_version"`
	// Source is bundled, dir or socket; Path the plugin's directory or
	// socket. Trust is bundled, unsigned or signed:<identity>; Isolation
	// none (a subprocess of loomuxd's own user) or container (a
	// sidecar).
	Source       string               `json:"source"`
	Path         string               `json:"path"`
	Trust        string               `json:"trust"`
	Isolation    string               `json:"isolation"`
	Capabilities []string             `json:"capabilities"`
	Permissions  []permissionResponse `json:"permissions"`
	// ConfigSchema is the manifest's configuration schema, for the
	// install form.
	ConfigSchema any `json:"config_schema"`
	// Installed lists the ids of the instances installed from this
	// entry.
	Installed []string `json:"installed"`
}

type permissionResponse struct {
	Scope  string `json:"scope"`
	Detail string `json:"detail"`
}

type listAvailablePluginsResponse struct {
	Plugins []availablePluginResponse `json:"plugins"`
}

// pluginResponse is an installed plugin instance. Config carries every
// setting, with each secret as {"set": bool}.
type pluginResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Label            string `json:"label"`
	Version          string `json:"version"`
	AvailableVersion string `json:"available_version"`
	Protocol         string `json:"protocol"`
	Source           string `json:"source"`
	Path             string `json:"path"`
	Trust            string `json:"trust"`
	// Status is installing, installed, disabled or error; StatusReason
	// says why when error.
	Status       string         `json:"status"`
	StatusReason string         `json:"status_reason"`
	Enabled      bool           `json:"enabled"`
	Capabilities []string       `json:"capabilities"`
	Config       map[string]any `json:"config"`
	// SecretsUnreadable: the stored secrets don't decrypt with this
	// server's master key.
	SecretsUnreadable bool `json:"secrets_unreadable"`
	// Check is the last check's result; null if none ran yet.
	Check *checkResponse `json:"check"`
	// Instance is the running instance's state (running, restarting,
	// failed, stopped) or "" when none runs; InstanceReason says why
	// when not running.
	Instance       string    `json:"instance"`
	InstanceReason string    `json:"instance_reason"`
	Machines       int       `json:"machines"`
	InstalledAt    time.Time `json:"installed_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type checkResponse struct {
	OK       bool              `json:"ok"`
	Problems []problemResponse `json:"problems"`
}

type problemResponse struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type listPluginsResponse struct {
	Plugins []pluginResponse `json:"plugins"`
}

// installPluginRequest: source defaults to bundled.
type installPluginRequest struct {
	Plugin string         `json:"plugin"`
	Source string         `json:"source"`
	Label  string         `json:"label"`
	Config map[string]any `json:"config"`
}

// setPluginConfigRequest: a secret left out keeps its stored value, ""
// clears it.
type setPluginConfigRequest struct {
	Config map[string]any `json:"config"`
}

func newCheckResponse(r *protocol.CheckResult) *checkResponse {
	if r == nil {
		return nil
	}
	out := &checkResponse{OK: r.OK, Problems: make([]problemResponse, 0, len(r.Problems))}
	for _, p := range r.Problems {
		out.Problems = append(out.Problems, problemResponse{Code: p.Code, Message: p.Message, Severity: p.Severity})
	}
	return out
}

func newPluginResponse(v *plugins.View) pluginResponse {
	config := v.Config
	if config == nil {
		config = map[string]any{}
	}
	return pluginResponse{
		ID: v.ID, Name: v.Name, Label: v.Label, Version: v.Version, AvailableVersion: v.AvailableVersion,
		Protocol: v.Protocol, Source: string(v.Source), Path: v.Path, Trust: v.Trust,
		Status: string(v.Status), StatusReason: v.StatusReason, Enabled: v.Enabled,
		Capabilities: append([]string{}, v.Capabilities...), Config: config, SecretsUnreadable: v.SecretsUnreadable,
		Check: newCheckResponse(v.Check), Instance: v.Instance.State, InstanceReason: v.Instance.Reason,
		Machines: v.Machines, InstalledAt: v.InstalledAt, UpdatedAt: v.UpdatedAt,
	}
}

func (s *Server) pluginsEnabled(w http.ResponseWriter) bool {
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "plugins are not available on this server")
		return false
	}
	return true
}

func (s *Server) handleListAvailablePlugins(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	available, err := s.plugins.Available(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list available plugins")
		return
	}
	installed, err := s.plugins.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list installed plugins")
		return
	}
	out := listAvailablePluginsResponse{Plugins: make([]availablePluginResponse, 0, len(available))}
	for _, a := range available {
		perms := make([]permissionResponse, 0, len(a.Manifest.Permissions))
		for _, p := range a.Manifest.Permissions {
			perms = append(perms, permissionResponse{Scope: p.Scope, Detail: p.Detail})
		}
		ids := []string{}
		for _, v := range installed {
			if v.Name == a.Manifest.Name && v.Source == a.Source {
				ids = append(ids, v.ID)
			}
		}
		var schema any
		if sch, err := a.Manifest.Schema(); err == nil {
			schema = sch
		}
		out.Plugins = append(out.Plugins, availablePluginResponse{
			Name: a.Manifest.Name, Title: a.Manifest.Title, Version: a.Manifest.Version, Protocol: a.Manifest.Protocol,
			Vendor: a.Manifest.Vendor, Homepage: a.Manifest.Homepage, Description: a.Manifest.Description,
			MinHostVersion: a.Manifest.MinHostVersion, Source: string(a.Source), Path: a.Path, Trust: a.Trust,
			Isolation: a.Isolation, Capabilities: append([]string{}, a.Manifest.Capabilities...), Permissions: perms,
			ConfigSchema: schema, Installed: ids,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	views, err := s.plugins.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list plugins")
		return
	}
	out := listPluginsResponse{Plugins: make([]pluginResponse, 0, len(views))}
	for _, v := range views {
		out.Plugins = append(out.Plugins, newPluginResponse(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	var req installPluginRequest
	if !readJSON(w, r, &req) {
		return
	}
	source := registry.PluginSource(req.Source)
	if source == "" {
		source = registry.PluginSourceBundled
	}
	switch source {
	case registry.PluginSourceBundled, registry.PluginSourceDir, registry.PluginSourceSocket:
	default:
		writeError(w, http.StatusBadRequest, "source must be bundled, dir or socket")
		return
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	v, err := s.plugins.Install(r.Context(), plugins.InstallRequest{Name: req.Plugin, Source: source, Label: req.Label, Config: req.Config})
	if !s.writePluginError(w, err, v) {
		return
	}
	writeJSON(w, http.StatusCreated, newPluginResponse(v))
}

func (s *Server) handleGetPlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	v, err := s.plugins.Get(r.Context(), r.PathValue("id"))
	if !s.writePluginError(w, err, v) {
		return
	}
	writeJSON(w, http.StatusOK, newPluginResponse(v))
}

func (s *Server) handleSetPluginConfig(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	var req setPluginConfigRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	v, err := s.plugins.SetConfig(r.Context(), r.PathValue("id"), req.Config)
	if !s.writePluginError(w, err, v) {
		return
	}
	writeJSON(w, http.StatusOK, newPluginResponse(v))
}

func (s *Server) handleEnablePlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	s.pluginAction(w, r, s.plugins.Enable)
}

func (s *Server) handleDisablePlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	s.pluginAction(w, r, s.plugins.Disable)
}

func (s *Server) handleUpgradePlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	s.pluginAction(w, r, s.plugins.Upgrade)
}

func (s *Server) handleCheckPlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	s.pluginAction(w, r, s.plugins.Check)
}

// pluginAction runs one of the lifecycle verbs and answers with the
// plugin. The handlers check pluginsEnabled before taking a method of
// s.plugins, which would be nil otherwise.
func (s *Server) pluginAction(w http.ResponseWriter, r *http.Request, action func(context.Context, string) (*plugins.View, error)) {
	v, err := action(r.Context(), r.PathValue("id"))
	if !s.writePluginError(w, err, v) {
		return
	}
	writeJSON(w, http.StatusOK, newPluginResponse(v))
}

func (s *Server) handleUninstallPlugin(w http.ResponseWriter, r *http.Request) {
	if !s.pluginsEnabled(w) {
		return
	}
	err := s.plugins.Uninstall(r.Context(), r.PathValue("id"), r.URL.Query().Get("targets"))
	var hasMachines *plugins.HasMachinesError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such plugin")
	case errors.Is(err, plugins.ErrBadUninstallMode):
		writeError(w, http.StatusBadRequest, "targets must be destroy or keep")
	case errors.As(err, &hasMachines):
		writeError(w, http.StatusConflict, hasMachines.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not uninstall the plugin")
	}
}

// writePluginError maps a manager error to a response and reports
// whether the caller may go on (err == nil). A *CheckFailedError means
// the row exists but the plugin can't work: 422 with the plugin as it
// is, so the client can show the reason and fix the configuration.
func (s *Server) writePluginError(w http.ResponseWriter, err error, v *plugins.View) bool {
	if err == nil {
		return true
	}
	var (
		configErr   *plugins.ConfigError
		checkFailed *plugins.CheckFailedError
		dropped     *plugins.CapabilityDroppedError
	)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such plugin")
	case errors.Is(err, plugins.ErrNotAvailable):
		writeError(w, http.StatusNotFound, "no such plugin is available to install")
	case errors.Is(err, plugins.ErrBadLabel):
		writeError(w, http.StatusBadRequest, "label: lowercase letters, digits and '-' only, at most 32, starting with a letter or digit")
	case errors.As(err, &configErr):
		writeError(w, http.StatusBadRequest, configErr.Error())
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, "a plugin with that label is already installed")
	case errors.Is(err, registry.ErrNoMasterKey):
		writeError(w, http.StatusServiceUnavailable, "plugin configuration needs LOOMUX_MASTER_KEY, which this server doesn't have")
	case errors.As(err, &dropped):
		writeError(w, http.StatusConflict, dropped.Error())
	case errors.As(err, &checkFailed) && v != nil:
		writeJSON(w, http.StatusUnprocessableEntity, newPluginResponse(v))
	case errors.Is(err, plugins.ErrUnavailable):
		writeError(w, http.StatusConflict, "the plugin isn't running")
	default:
		writeError(w, http.StatusInternalServerError, "plugin operation failed")
	}
	return false
}
