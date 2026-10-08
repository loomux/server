package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// SSHKeyStore backs /api/v1/ssh-keys (LOOM-138): the SSH keys Loomux
// manages for reaching targets. Satisfied by any registry.Store; it
// lists targets itself to say which ones use a key.
type SSHKeyStore interface {
	CreateSSHKey(ctx context.Context, k *registry.SSHKey) error
	ListSSHKeys(ctx context.Context) ([]*registry.SSHKey, error)
	DeleteSSHKey(ctx context.Context, id string) error
	ListTargets(ctx context.Context) ([]*registry.Target, error)
}

// WithSSHKeys enables /api/v1/ssh-keys (LOOM-138).
func WithSSHKeys(store SSHKeyStore) Option {
	return func(s *Server) { s.sshKeys = store }
}

// sshKeyResponse is a managed key's public parts. There is deliberately
// no field the private key could go in: the API never returns it.
type sshKeyResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	// PublicKey is the authorized_keys line to add on a target.
	PublicKey string `json:"public_key"`
	// Origin is "generated" or "imported".
	Origin    string    `json:"origin"`
	CreatedAt time.Time `json:"created_at"`
	// UsedBy are the ids of the targets that connect with this key.
	UsedBy []string `json:"used_by"`
}

func newSSHKeyResponse(k *registry.SSHKey, usedBy []string) sshKeyResponse {
	if usedBy == nil {
		usedBy = []string{}
	}
	return sshKeyResponse{ID: k.ID, Name: k.Name, Type: k.Type, Fingerprint: k.Fingerprint,
		PublicKey: k.PublicKey, Origin: k.Origin, CreatedAt: k.CreatedAt, UsedBy: usedBy}
}

type listSSHKeysResponse struct {
	SSHKeys []sshKeyResponse `json:"ssh_keys"`
}

type createSSHKeyRequest struct {
	Name string `json:"name"`
}

func (s *Server) sshKeysEnabled(w http.ResponseWriter) bool {
	if s.sshKeys == nil {
		writeError(w, http.StatusNotFound, "SSH keys are not available on this server")
		return false
	}
	return true
}

// handleListSSHKeys: GET /api/v1/ssh-keys. Decrypts nothing, so it works
// without (or with a wrong) master key.
func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	if !s.sshKeysEnabled(w) {
		return
	}
	keys, err := s.sshKeys.ListSSHKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list SSH keys")
		return
	}
	ts, err := s.sshKeys.ListTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list targets")
		return
	}
	usedBy := map[string][]string{}
	for _, t := range ts {
		if t.SSHKeyRef != "" {
			usedBy[t.SSHKeyRef] = append(usedBy[t.SSHKeyRef], t.ID)
		}
	}
	out := make([]sshKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, newSSHKeyResponse(k, usedBy[k.ID]))
	}
	writeJSON(w, http.StatusOK, listSSHKeysResponse{SSHKeys: out})
}

// handleCreateSSHKey: POST /api/v1/ssh-keys {name} generates an ed25519
// key and returns its public parts, the public key to authorize on a
// target among them.
func (s *Server) handleCreateSSHKey(w http.ResponseWriter, r *http.Request) {
	if !s.sshKeysEnabled(w) {
		return
	}
	var req createSSHKeyRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !targets.ValidSSHKeyName(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be letters, digits, '.', '_' or '-', starting with a letter or digit, at most 64")
		return
	}
	k, err := targets.GenerateSSHKey(uuid.NewString(), req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate SSH key")
		return
	}
	if err := s.sshKeys.CreateSSHKey(r.Context(), k); err != nil {
		switch {
		case errors.Is(err, registry.ErrNoMasterKey):
			writeError(w, http.StatusServiceUnavailable, "storing SSH keys needs LOOMUX_MASTER_KEY, which this server doesn't have")
		case errors.Is(err, registry.ErrConflict):
			writeError(w, http.StatusConflict, "an SSH key with that name already exists")
		default:
			writeError(w, http.StatusInternalServerError, "could not store SSH key")
		}
		return
	}
	writeJSON(w, http.StatusCreated, newSSHKeyResponse(k, nil))
}

// handleDeleteSSHKey: DELETE /api/v1/ssh-keys/{id}; 409 while a target
// uses the key.
func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	if !s.sshKeysEnabled(w) {
		return
	}
	if err := s.sshKeys.DeleteSSHKey(r.Context(), r.PathValue("id")); err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			writeError(w, http.StatusNotFound, "no such SSH key")
		case errors.Is(err, registry.ErrConflict):
			writeError(w, http.StatusConflict, "a target still uses this SSH key; give it another key first")
		default:
			writeError(w, http.StatusInternalServerError, "could not delete SSH key")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
