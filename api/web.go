package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Loomux/server/webbundle"
)

// WebBundles is the web client's bundle manager (LOOM-118,
// webbundle.Manager): which bundle to serve, and updating it in place.
type WebBundles interface {
	Root() string
	Current() *webbundle.Release
	Previous() *webbundle.Release
	Installed() bool
	Enabled() bool
	Latest(ctx context.Context) (*webbundle.Release, error)
	UpdateAvailable(latest *webbundle.Release) bool
	Install(ctx context.Context) (*webbundle.Release, error)
	Rollback() (*webbundle.Release, error)
}

// WithWebBundles serves the web client from whichever bundle web says is
// current, read per request so an update or rollback takes effect at
// once, and enables /api/v1/web/*. It takes the place of WithStaticDir.
func WithWebBundles(web WebBundles) Option {
	return func(s *Server) { s.web = web }
}

type webVersionResponse struct {
	// Current is the bundle served; null for an image bundle from before
	// releases (LOOM-58) that doesn't say what it is.
	Current *webbundle.Release `json:"current"`
	// Source is "image" for the bundle the image was built with,
	// "installed" for one installed since.
	Source          string             `json:"source"`
	Previous        *webbundle.Release `json:"previous,omitempty"`
	Latest          *webbundle.Release `json:"latest,omitempty"`
	LatestError     string             `json:"latest_error,omitempty"`
	UpdateAvailable bool               `json:"update_available"`
	UpdatesEnabled  bool               `json:"updates_enabled"`
}

func (s *Server) webVersion(ctx context.Context, withLatest bool) webVersionResponse {
	if s.web == nil {
		return webVersionResponse{Source: "image"}
	}
	resp := webVersionResponse{
		Current: s.web.Current(), Source: "image", Previous: s.web.Previous(), UpdatesEnabled: s.web.Enabled(),
	}
	if s.web.Installed() {
		resp.Source = "installed"
	}
	if withLatest && resp.UpdatesEnabled {
		latest, err := s.web.Latest(ctx)
		if err != nil {
			resp.LatestError = err.Error()
		} else {
			resp.Latest = latest
			resp.UpdateAvailable = s.web.UpdateAvailable(latest)
		}
	}
	return resp
}

// handleWebVersion: GET /api/v1/web/version — the bundle served, and the
// newest published one when updates are configured.
func (s *Server) handleWebVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.webVersion(r.Context(), true))
}

// handleWebUpdate: POST /api/v1/web/update — install the newest release
// if it is newer than the bundle served. Pages already open keep the old
// bundle until they reload.
func (s *Server) handleWebUpdate(w http.ResponseWriter, r *http.Request) {
	if s.web == nil || !s.web.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "web updates are not configured on this server")
		return
	}
	_, err := s.web.Install(r.Context())
	switch {
	case errors.Is(err, webbundle.ErrUpToDate):
		writeError(w, http.StatusConflict, "the web client is already up to date")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "update failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.webVersion(r.Context(), false))
}

// handleWebRollback: POST /api/v1/web/rollback — switch back to the
// bundle the last update replaced.
func (s *Server) handleWebRollback(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		writeError(w, http.StatusConflict, "there is no previous web client to roll back to")
		return
	}
	if _, err := s.web.Rollback(); err != nil {
		if errors.Is(err, webbundle.ErrNoPrevious) {
			writeError(w, http.StatusConflict, "there is no previous web client to roll back to")
			return
		}
		writeError(w, http.StatusInternalServerError, "rollback failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.webVersion(r.Context(), false))
}
