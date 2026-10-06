package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// CredentialStore backs /api/v1/credentials (LOOM-134): the vault of
// secrets injected into agents' environments per launch (design §7).
// Values go in and are never read back out through the API.
type CredentialStore interface {
	CreateCredential(ctx context.Context, c *registry.Credential) error
	ListCredentialInfo(ctx context.Context) ([]*registry.Credential, error)
	SetCredentialValue(ctx context.Context, id, value string) error
	DeleteCredential(ctx context.Context, id string) error
}

// WithCredentials enables /api/v1/credentials (LOOM-134).
func WithCredentials(c CredentialStore) Option {
	return func(s *Server) { s.credentials = c }
}

// credentialName is what a credential's name must be: it becomes an
// environment variable in the agent's launch shell.
var credentialName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// reservedCredentialNames would change how the agent's shell itself runs,
// not hand it a secret; LOOMUX_* are Loomux's own.
var reservedCredentialNames = []string{"PATH", "HOME", "SHELL", "USER", "LOGNAME", "PWD", "IFS", "ENV", "BASH_ENV", "LD_PRELOAD", "LD_LIBRARY_PATH", "TMUX"}

// maxCredentialValue bounds one secret; an API key or token is far
// smaller.
const maxCredentialValue = 64 << 10

// credentialResponse is a credential without its value: the API never
// returns one.
type credentialResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	AgentType   string    `json:"agent_type,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func newCredentialResponse(c *registry.Credential) credentialResponse {
	return credentialResponse{ID: c.ID, Name: c.Name, WorkspaceID: c.WorkspaceID, AgentType: c.AgentType, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

type listCredentialsResponse struct {
	Credentials []credentialResponse `json:"credentials"`
}

type createCredentialRequest struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	WorkspaceID string `json:"workspace_id"`
	AgentType   string `json:"agent_type"`
}

type setCredentialValueRequest struct {
	Value string `json:"value"`
}

func (s *Server) credentialsEnabled(w http.ResponseWriter) bool {
	if s.credentials == nil {
		writeError(w, http.StatusNotFound, "credentials are not available on this server")
		return false
	}
	return true
}

// validCredentialValue says what's wrong with value, "" if nothing.
func validCredentialValue(value string) string {
	switch {
	case value == "":
		return "value is required"
	case len(value) > maxCredentialValue:
		return "value is too long (at most 64 KiB)"
	case strings.ContainsRune(value, 0):
		return "value can't contain a NUL byte"
	}
	return ""
}

// handleListCredentials: GET /api/v1/credentials — names and scopes only.
// Read without decrypting, so it works even when the master key no longer
// matches and a broken row needs deleting.
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsEnabled(w) {
		return
	}
	creds, err := s.credentials.ListCredentialInfo(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list credentials")
		return
	}
	out := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		out = append(out, newCredentialResponse(c))
	}
	writeJSON(w, http.StatusOK, listCredentialsResponse{Credentials: out})
}

// handleCreateCredential: POST /api/v1/credentials. One name per scope
// (none, a workspace, an agent type, or both); the most specific scope
// wins when an agent launches.
func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsEnabled(w) {
		return
	}
	var req createCredentialRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	req.AgentType = strings.TrimSpace(req.AgentType)
	switch {
	case !credentialName.MatchString(req.Name):
		writeError(w, http.StatusBadRequest, "name must be an environment variable name: letters, digits and underscores, not starting with a digit")
		return
	case slices.Contains(reservedCredentialNames, strings.ToUpper(req.Name)) || strings.HasPrefix(strings.ToUpper(req.Name), "LOOMUX_"):
		writeError(w, http.StatusBadRequest, "that name is used by the shell or by Loomux itself")
		return
	case req.AgentType != "" && len(s.agentTypes) > 0 && !slices.Contains(s.agentTypes, req.AgentType):
		writeError(w, http.StatusBadRequest, "unknown agent_type")
		return
	}
	if msg := validCredentialValue(req.Value); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	c := &registry.Credential{ID: uuid.NewString(), Name: req.Name, WorkspaceID: req.WorkspaceID, AgentType: req.AgentType, Value: req.Value}
	if err := s.credentials.CreateCredential(r.Context(), c); err != nil {
		switch {
		case errors.Is(err, registry.ErrConflict) && req.WorkspaceID != "" && strings.Contains(err.Error(), "does not exist"):
			writeError(w, http.StatusBadRequest, "no such workspace")
		case errors.Is(err, registry.ErrConflict):
			writeError(w, http.StatusConflict, "a credential with that name already exists at this scope; set its value instead")
		default:
			writeError(w, http.StatusInternalServerError, "could not store credential")
		}
		return
	}
	writeJSON(w, http.StatusCreated, newCredentialResponse(c))
}

// handleSetCredentialValue: PUT /api/v1/credentials/{id}/value — rotate a
// secret in place, keeping its name and scope.
func (s *Server) handleSetCredentialValue(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsEnabled(w) {
		return
	}
	var req setCredentialValueRequest
	if !readJSON(w, r, &req) {
		return
	}
	if msg := validCredentialValue(req.Value); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.credentials.SetCredentialValue(r.Context(), r.PathValue("id"), req.Value); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such credential")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not store credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteCredential: DELETE /api/v1/credentials/{id}.
func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsEnabled(w) {
		return
	}
	if err := s.credentials.DeleteCredential(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such credential")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not delete credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
