// Tests for the release scripts the image workflow runs (LOOM-194):
// deploy/ghcr.sh against a fake registry, deploy/publish-image.sh with
// a fake docker. Shell, so the workflow runs exactly what is tested.
package deploy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeRegistry serves the token, manifest and blob endpoints ghcr.sh
// reads, for one repository loomux/agent:
//
//	main    a single docker manifest, digest sha256:man1, config labels
//	multi   an index: an amd64 manifest (sha256:amd) and an attestation
//	missing 404
func fakeRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:loomux/agent:pull" {
			http.Error(w, "bad scope "+r.URL.Query().Get("scope"), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:cfg","mediaType":"application/vnd.docker.container.image.v1+json","size":1}}`
	mux.HandleFunc("GET /v2/loomux/agent/manifests/{ref}", authed(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "accept lacks the index type", http.StatusBadRequest)
			return
		}
		switch r.PathValue("ref") {
		case "main", "sha256:amd":
			d := "sha256:man1"
			if r.PathValue("ref") == "sha256:amd" {
				d = "sha256:amd"
			}
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", d)
			w.Write([]byte(manifest))
		case "multi":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:idx")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
			  {"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}},
			  {"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	mux.HandleFunc("GET /v2/loomux/agent/blobs/{digest}", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("digest") != "sha256:cfg" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"config":{"Labels":{"org.opencontainers.image.revision":"abc123","org.opencontainers.image.version":"0.3.5"}}}`))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func ghcr(t *testing.T, registry string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"./ghcr.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "GHCR_REGISTRY="+registry)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("ghcr.sh %v: %v", args, err)
	}
	if stderr.Len() > 0 {
		t.Logf("ghcr.sh %v stderr: %s", args, stderr.String())
	}
	return strings.TrimSpace(string(out)), code
}

func TestGhcrDigest(t *testing.T) {
	srv := fakeRegistry(t)
	if got, code := ghcr(t, srv.URL, "digest", "ghcr.io/loomux/agent", "main"); got != "sha256:man1" || code != 0 {
		t.Errorf("digest main = %q, %d", got, code)
	}
	// An index: the digest is the index's (what the tag names), not a
	// platform manifest's.
	if got, code := ghcr(t, srv.URL, "digest", "ghcr.io/loomux/agent", "multi"); got != "sha256:idx" || code != 0 {
		t.Errorf("digest multi = %q, %d", got, code)
	}
	if got, code := ghcr(t, srv.URL, "digest", "ghcr.io/loomux/agent", "missing"); got != "" || code != 2 {
		t.Errorf("digest missing = %q, %d; want nothing and exit 2", got, code)
	}
}

func TestGhcrLabels(t *testing.T) {
	srv := fakeRegistry(t)
	if got, code := ghcr(t, srv.URL, "label", "ghcr.io/loomux/agent", "main", "org.opencontainers.image.version"); got != "0.3.5" || code != 0 {
		t.Errorf("label version = %q, %d", got, code)
	}
	// Through an index: the amd64 image's config.
	if got, code := ghcr(t, srv.URL, "label", "ghcr.io/loomux/agent", "multi", "org.opencontainers.image.revision"); got != "abc123" || code != 0 {
		t.Errorf("label revision via index = %q, %d", got, code)
	}
	if got, code := ghcr(t, srv.URL, "label", "ghcr.io/loomux/agent", "main", "nope"); got != "" || code != 0 {
		t.Errorf("an unset label = %q, %d; want empty and exit 0", got, code)
	}
	if got, code := ghcr(t, srv.URL, "label", "ghcr.io/loomux/agent", "missing", "org.opencontainers.image.version"); got != "" || code != 2 {
		t.Errorf("label of a missing tag = %q, %d; want exit 2", got, code)
	}
	got, code := ghcr(t, srv.URL, "labels", "ghcr.io/loomux/agent", "main")
	var labels map[string]string
	if code != 0 || json.Unmarshal([]byte(got), &labels) != nil || labels["org.opencontainers.image.revision"] != "abc123" {
		t.Errorf("labels main = %q, %d", got, code)
	}
}
