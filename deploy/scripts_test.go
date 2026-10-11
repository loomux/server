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

// publishHarness is a fake docker on PATH and a fake ghcr.sh for
// publish-image.sh: docker push reports a digest derived from the ref,
// every docker call is logged; the fake ghcr answers from fixture files
// <fix>/<image name>/<tag>.digest and .revision.
type publishHarness struct {
	bin, fix, log string
}

func newPublishHarness(t *testing.T) *publishHarness {
	t.Helper()
	dir := t.TempDir()
	h := &publishHarness{bin: dir + "/bin", fix: dir + "/fix", log: dir + "/docker.log"}
	os.MkdirAll(h.bin, 0o755)
	os.MkdirAll(h.fix, 0o755)
	docker := `#!/bin/sh
echo "docker $*" >> "$FAKE_LOG"
case "$1" in
push) printf 'The push refers to repository\nlatest: digest: sha256:%s size: 1\n' "$(printf '%s' "$2" | sha256sum | cut -c1-64)" ;;
esac
`
	ghcr := `#!/bin/sh
f="$FIX/$(basename "$2")/$3"
case "$1" in
digest) [ -f "$f.digest" ] || exit 2; cat "$f.digest" ;;
label)
  [ -f "$f.digest" ] || exit 2
  case "$4" in
  org.opencontainers.image.revision) cat "$f.revision" ;;
  org.opencontainers.image.version) cat "$f.version" ;;
  esac ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(h.bin+"/docker", []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.bin+"/ghcr", []byte(ghcr), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// existing records that <image name>:<tag> exists with digest, revision
// and version label.
func (h *publishHarness) existing(t *testing.T, name, tag, digest, revision, version string) {
	t.Helper()
	os.MkdirAll(h.fix+"/"+name, 0o755)
	os.WriteFile(h.fix+"/"+name+"/"+tag+".digest", []byte(digest+"\n"), 0o644)
	os.WriteFile(h.fix+"/"+name+"/"+tag+".revision", []byte(revision+"\n"), 0o644)
	os.WriteFile(h.fix+"/"+name+"/"+tag+".version", []byte(version+"\n"), 0o644)
}

func (h *publishHarness) run(t *testing.T, args ...string) (stdout string, code int, log string) {
	t.Helper()
	os.Remove(h.log)
	cmd := exec.Command("sh", append([]string{"./publish-image.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "GHCR="+h.bin+"/ghcr", "FAKE_LOG="+h.log, "FIX="+h.fix)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("publish-image.sh %v: %v", args, err)
	}
	if stderr.Len() > 0 {
		t.Logf("publish-image.sh %v stderr:\n%s", args, stderr.String())
	}
	b, _ := os.ReadFile(h.log)
	return strings.TrimSpace(string(out)), code, string(b)
}

// pushedDigest is what the fake docker reports for a push of ref.
func pushedDigest(ref string) string {
	out, _ := exec.Command("sh", "-c", "printf '%s' \""+ref+"\" | sha256sum | cut -c1-64").Output()
	return "sha256:" + strings.TrimSpace(string(out))
}

const agent = "ghcr.io/loomux/agent"

func TestPublishImageFresh(t *testing.T) {
	h := newPublishHarness(t)
	out, code, log := h.run(t, agent, "rev1", "0.3.80", "agent:test", "sha-abc1234", "0.3.80", "main")
	want := pushedDigest(agent + ":sha-abc1234")
	if code != 0 || out != want {
		t.Fatalf("stdout %q, exit %d; want the pushed digest %s", out, code, want)
	}
	for _, line := range []string{
		"docker tag agent:test " + agent + ":sha-abc1234",
		"docker push " + agent + ":sha-abc1234",
		"docker buildx imagetools create -t " + agent + ":0.3.80 " + agent + "@" + want,
		"docker buildx imagetools create -t " + agent + ":main " + agent + "@" + want,
	} {
		if !strings.Contains(log, line+"\n") {
			t.Errorf("missing %q in\n%s", line, log)
		}
	}
	if strings.Count(log, "docker push") != 1 {
		t.Errorf("one push expected:\n%s", log)
	}
}

func TestPublishImageRerunAdoptsTheExistingShaTag(t *testing.T) {
	h := newPublishHarness(t)
	h.existing(t, "agent", "sha-abc1234", "sha256:earlier", "rev1", "0.3.80")
	out, code, log := h.run(t, agent, "rev1", "0.3.80", "agent:test", "sha-abc1234", "0.3.80", "main")
	if code != 0 || out != "sha256:earlier" {
		t.Fatalf("stdout %q, exit %d; want the existing digest", out, code)
	}
	if strings.Contains(log, "docker push") {
		t.Errorf("nothing should be pushed on a re-run:\n%s", log)
	}
	for _, tag := range []string{"0.3.80", "main"} {
		if !strings.Contains(log, "docker buildx imagetools create -t "+agent+":"+tag+" "+agent+"@sha256:earlier\n") {
			t.Errorf("%s not pointed at the existing digest:\n%s", tag, log)
		}
	}
}

func TestPublishImageImmutableTagForAnotherCommitFails(t *testing.T) {
	h := newPublishHarness(t)
	h.existing(t, "agent", "0.3.80", "sha256:other", "rev0", "0.3.80")
	out, code, log := h.run(t, agent, "rev1", "0.3.80", "agent:test", "sha-abc1234", "0.3.80", "main")
	if code == 0 {
		t.Fatalf("exit 0 with stdout %q; want a failure", out)
	}
	if strings.Contains(log, "docker push") || strings.Contains(log, "imagetools create") {
		t.Errorf("nothing may be pushed or re-tagged:\n%s", log)
	}
}

func TestPublishImageTwoExistingTagsMustAgree(t *testing.T) {
	h := newPublishHarness(t)
	h.existing(t, "agent", "sha-abc1234", "sha256:e1", "rev1", "0.3.80")
	h.existing(t, "agent", "0.3.80", "sha256:e2", "rev1", "0.3.80")
	if _, code, log := h.run(t, agent, "rev1", "0.3.80", "agent:test", "sha-abc1234", "0.3.80"); code == 0 || strings.Contains(log, "imagetools create") {
		t.Errorf("two digests for one commit must fail (exit %d):\n%s", code, log)
	}
}

func TestPublishImageMovingTagIsRepointed(t *testing.T) {
	h := newPublishHarness(t)
	h.existing(t, "server", "main", "sha256:old", "rev0", "0.3.79")
	server := "ghcr.io/loomux/server"
	out, code, log := h.run(t, server, "rev1", "0.3.80", "server:test", "abc1234", "sha-abc1234", "main")
	want := pushedDigest(server + ":abc1234")
	if code != 0 || out != want {
		t.Fatalf("stdout %q, exit %d; want %s", out, code, want)
	}
	// The legacy bare tag is immutable too, and was the one pushed.
	for _, line := range []string{
		"docker push " + server + ":abc1234",
		"docker buildx imagetools create -t " + server + ":sha-abc1234 " + server + "@" + want,
		"docker buildx imagetools create -t " + server + ":main " + server + "@" + want,
	} {
		if !strings.Contains(log, line+"\n") {
			t.Errorf("missing %q in\n%s", line, log)
		}
	}
}

func TestPublishImageNeedsArguments(t *testing.T) {
	h := newPublishHarness(t)
	if _, code, _ := h.run(t, agent, "rev1", "0.3.80", "agent:test"); code == 0 {
		t.Error("no tags should be refused")
	}
}

// A hand-pushed MINOR on a commit the merge already released as a
// patch: the commit tags exist for this revision under the patch's
// version label, so they are left alone (neither adopted nor
// re-pushed), and the new build, made as the MINOR, is pushed under the
// version tag (review of #353).
func TestPublishImageSameCommitOtherVersionPushesTheNewBuild(t *testing.T) {
	h := newPublishHarness(t)
	server := "ghcr.io/loomux/server"
	h.existing(t, "server", "abc1234", "sha256:patchbuild", "rev1", "0.3.80")
	h.existing(t, "server", "sha-abc1234", "sha256:patchbuild", "rev1", "0.3.80")
	out, code, log := h.run(t, server, "rev1", "0.4.0", "server:test", "abc1234", "sha-abc1234", "0.4.0")
	want := pushedDigest(server + ":0.4.0")
	if code != 0 || out != want {
		t.Fatalf("stdout %q, exit %d; want the new build's digest %s", out, code, want)
	}
	if !strings.Contains(log, "docker push "+server+":0.4.0\n") {
		t.Errorf("the MINOR wasn't pushed:\n%s", log)
	}
	if strings.Contains(log, "imagetools create") || strings.Contains(log, "patchbuild") {
		t.Errorf("the patch's commit tags must be left alone:\n%s", log)
	}
}

// A run without a version (a plain rebuild) of a commit that was
// released: its commit tags exist under the release's label; nothing
// is pushed or moved, and the run reports the commit's existing digest.
func TestPublishImageNothingNewReportsTheExistingDigest(t *testing.T) {
	h := newPublishHarness(t)
	server := "ghcr.io/loomux/server"
	h.existing(t, "server", "abc1234", "sha256:released", "rev1", "0.3.80")
	h.existing(t, "server", "sha-abc1234", "sha256:released", "rev1", "0.3.80")
	out, code, log := h.run(t, server, "rev1", "sha-abc1234", "server:test", "abc1234", "sha-abc1234")
	if code != 0 || out != "sha256:released" {
		t.Fatalf("stdout %q, exit %d, log:\n%s", out, code, log)
	}
	if strings.Contains(log, "docker push") || strings.Contains(log, "imagetools create") {
		t.Errorf("nothing may be pushed or moved:\n%s", log)
	}
}

// The server image's version label must be what the publish step hands
// publish-image.sh as this run's version label, or a re-run after a
// partial failure of a run without a version leaves the bare :<short>
// (labelled one way) alone and pushes a second digest as :sha-<short>
// (review of #353): every image labels the release, or sha-<short>.
func TestImageWorkflowServerVersionLabelMatchesThePublishStep(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(data)
	label := "org.opencontainers.image.version=${{ steps.meta.outputs.release || format('sha-{0}', steps.meta.outputs.sha) }}"
	if !strings.Contains(wf, label) {
		t.Errorf("image.yml doesn't label the server image %q", label)
	}
	if !strings.Contains(wf, `"${VERSION:-sha-${short}}"`) {
		t.Error("image.yml's publish step doesn't pass ${VERSION:-sha-${short}} as the version label")
	}
}
