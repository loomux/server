package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Loomux/server/api"
)

// TestVersion_ReturnsServerAndAPIVersion covers design spec §10 axis 4
// (server semver, independent of the API version) — an unauthenticated
// endpoint so clients can check compatibility before logging in.
func TestVersion_ReturnsServerAndAPIVersion(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/version")
	if err != nil {
		t.Fatalf("GET /api/v1/version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		ServerVersion string `json:"server_version"`
		APIVersion    string `json:"api_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.APIVersion != api.APIVersion {
		t.Errorf("api_version = %q, want %q", out.APIVersion, api.APIVersion)
	}
	if out.ServerVersion == "" {
		t.Error("server_version is empty")
	}
}

// TestUnsupportedAPIPath_ReturnsStructuredRejection covers design spec
// §10 axis 1's "a mismatch is a clear rejection or warning — not silent
// breakage" — a client hitting an unknown/future API version gets a
// structured body naming what's actually supported, not a bare 404.
func TestUnsupportedAPIPath_ReturnsStructuredRejection(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/v2/dispatch")
	if err != nil {
		t.Fatalf("GET /api/v2/dispatch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	var out struct {
		Error             string   `json:"error"`
		SupportedVersions []string `json:"supported_versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v (body isn't structured JSON)", err)
	}
	if out.Error == "" {
		t.Error("error field is empty")
	}
	if len(out.SupportedVersions) != 1 || out.SupportedVersions[0] != api.APIVersion {
		t.Errorf("supported_versions = %v, want [%q]", out.SupportedVersions, api.APIVersion)
	}
}

// TestWrongMethod_OnKnownPath_Returns405NotTheCatchAll proves the
// unsupported-version catch-all doesn't shadow a real, registered path
// hit with the wrong HTTP method — that's a distinct "you have the
// right version but the wrong verb" case, not a version mismatch.
func TestWrongMethod_OnKnownPath_Returns405NotTheCatchAll(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/login") // login is POST-only
	if err != nil {
		t.Fatalf("GET /api/v1/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
