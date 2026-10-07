package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Target onboarding through the API (LOOM-114): scan a target's host
// key, pin one of the scanned keys by fingerprint, unpin, and test the
// target. A scan trusts nothing: only a pin naming a fingerprint from the
// target's latest scan does.
//
// Loomux has a single user today, so any session may scan and pin. If it
// ever has several, these endpoints must become admin-only: whoever pins
// decides which machine every later command on that target reaches.

// HostKeyScanner reads the host key a target presents, without trusting
// it — targets.ScanHostKey.
type HostKeyScanner func(ctx context.Context, t *registry.Target) ([]targets.HostKey, error)

// HostKeyStore is where pins are kept — satisfied by any registry.Store.
type HostKeyStore interface {
	SetTargetHostKeys(ctx context.Context, id, hostKeys string) error
}

// WithHostKeyPinning enables POST /targets/{id}/scan-host-key and
// POST/DELETE /targets/{id}/pin (LOOM-114).
func WithHostKeyPinning(scan HostKeyScanner, store HostKeyStore) Option {
	return func(s *Server) {
		s.scanHostKey, s.hostKeys = scan, store
		s.scans = &scanResults{byTarget: map[string]scanResult{}}
	}
}

// scanTTL is how long a scan's keys can be pinned.
const scanTTL = 10 * time.Minute

type scanResult struct {
	keys    []targets.HostKey
	expires time.Time
}

// scanResults holds each target's latest scan, in memory: a restart
// forgets them, and the user scans again.
type scanResults struct {
	mu       sync.Mutex
	byTarget map[string]scanResult
}

type hostKeyResponse struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

func hostKeyResponses(keys []targets.HostKey) []hostKeyResponse {
	out := make([]hostKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, hostKeyResponse{Type: k.Type, Fingerprint: k.Fingerprint})
	}
	return out
}

func pinnedHostKeys(t *registry.Target) []hostKeyResponse {
	keys, _ := targets.ParseHostKeys(t.HostKeys)
	return hostKeyResponses(keys)
}

// remoteTarget fetches the path's target for a host-key endpoint,
// writing the error response itself when there isn't a remote one.
func (s *Server) remoteTarget(w http.ResponseWriter, r *http.Request) (*registry.Target, bool) {
	if s.scanHostKey == nil {
		writeError(w, http.StatusNotImplemented, "host key pinning is not configured on this server")
		return nil, false
	}
	t, err := s.targets.GetTarget(r.Context(), r.PathValue("id"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch target")
		return nil, false
	}
	if t.Kind != registry.TargetKindRemote {
		writeError(w, http.StatusBadRequest, "only a remote target has a host key")
		return nil, false
	}
	return t, true
}

type scanHostKeyResponse struct {
	TargetID string            `json:"target_id"`
	HostKeys []hostKeyResponse `json:"host_keys"`
	// ExpiresAt is until when one of HostKeys can be pinned.
	ExpiresAt time.Time `json:"expires_at"`
	// Pinned is what the target has pinned now.
	Pinned []hostKeyResponse `json:"pinned_host_keys"`
}

// handleScanHostKey connects to the target and reports the host key it
// presents. Nothing is trusted: compare the fingerprint with the
// machine's own (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub, run
// there) before pinning it.
func (s *Server) handleScanHostKey(w http.ResponseWriter, r *http.Request) {
	t, ok := s.remoteTarget(w, r)
	if !ok {
		return
	}
	keys, err := s.scanHostKey(r.Context(), t)
	if err != nil {
		var unreachable *targets.UnreachableError
		if errors.As(err, &unreachable) {
			writeError(w, http.StatusBadGateway, "could not read the host key: "+unreachable.Hint())
			return
		}
		writeError(w, http.StatusBadGateway, "could not read the host key")
		return
	}
	expires := time.Now().Add(scanTTL).UTC()
	s.scans.mu.Lock()
	s.scans.byTarget[t.ID] = scanResult{keys: keys, expires: expires}
	s.scans.mu.Unlock()
	writeJSON(w, http.StatusOK, scanHostKeyResponse{TargetID: t.ID, HostKeys: hostKeyResponses(keys),
		ExpiresAt: expires, Pinned: pinnedHostKeys(t)})
}

// pinHostKeyRequest is POST /targets/{id}/pin's body.
type pinHostKeyRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// handlePinHostKey pins the key with the given fingerprint from the
// target's latest scan: from then on the target is checked against it
// alone, ahead of the mounted known_hosts.
func (s *Server) handlePinHostKey(w http.ResponseWriter, r *http.Request) {
	t, ok := s.remoteTarget(w, r)
	if !ok {
		return
	}
	var req pinHostKeyRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.scans.mu.Lock()
	scan, scanned := s.scans.byTarget[t.ID]
	s.scans.mu.Unlock()
	if !scanned || time.Now().After(scan.expires) {
		writeError(w, http.StatusConflict, "scan the target's host key first (POST /api/v1/targets/{id}/scan-host-key); "+
			"a scan can be pinned for 10 minutes")
		return
	}
	var line string
	for _, k := range scan.keys {
		if k.Fingerprint == strings.TrimSpace(req.Fingerprint) {
			line = k.Line
		}
	}
	if line == "" {
		writeError(w, http.StatusConflict, "that fingerprint isn't one the latest scan of this target returned")
		return
	}
	if err := s.hostKeys.SetTargetHostKeys(r.Context(), t.ID, line); err != nil {
		writeError(w, http.StatusInternalServerError, "could not pin the host key")
		return
	}
	s.scans.mu.Lock()
	delete(s.scans.byTarget, t.ID)
	s.scans.mu.Unlock()
	s.writeStoredTarget(w, r, t.ID)
}

// handleUnpinHostKey removes the target's pin: it is checked against the
// mounted known_hosts again.
func (s *Server) handleUnpinHostKey(w http.ResponseWriter, r *http.Request) {
	t, ok := s.remoteTarget(w, r)
	if !ok {
		return
	}
	if err := s.hostKeys.SetTargetHostKeys(r.Context(), t.ID, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "could not unpin the host key")
		return
	}
	s.writeStoredTarget(w, r, t.ID)
}

func (s *Server) writeStoredTarget(w http.ResponseWriter, r *http.Request, id string) {
	stored, err := s.targets.GetTarget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read back the target")
		return
	}
	writeJSON(w, http.StatusOK, newTargetResponse(stored))
}

type testTargetResponse struct {
	TargetID    string `json:"target_id"`
	Reachable   bool   `json:"reachable"`
	TmuxVersion string `json:"tmux_version,omitempty"`
	LatencyMS   int64  `json:"latency_ms"`
	Error       string `json:"error,omitempty"`
	// HostKeyProblem is set when the target's host key changed or isn't
	// known: scan and pin it (or fix the SSH secret's known_hosts).
	HostKeyProblem bool `json:"host_key_problem"`
}

// handleTestTarget checks a target now: can Loomux reach it over SSH, and
// run tmux there. A host key problem is flagged, for the re-pin flow.
func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	if s.targetProber == nil {
		writeError(w, http.StatusNotImplemented, "target probing is not configured on this server")
		return
	}
	h, _, err := s.targetProber.ProbeTarget(r.Context(), r.PathValue("id"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not test target")
		return
	}
	writeJSON(w, http.StatusOK, testTargetResponse{
		TargetID: h.TargetID, Reachable: h.Reachable && h.TmuxVersion != "", TmuxVersion: h.TmuxVersion,
		LatencyMS: h.Latency.Milliseconds(), Error: h.Error,
		HostKeyProblem: strings.Contains(h.Error, string(targets.SSHHostKeyChanged)) ||
			strings.Contains(h.Error, string(targets.SSHHostKeyUnknown)),
	})
}
