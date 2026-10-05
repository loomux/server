package webbundle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHub serves loomux/server's pin (deploy/web-ref, deploy/web-sha256
// on main) and loomux/web's releases by tag; a tarball asset redirects to
// a separate storage host, as GitHub's do. releases maps a tag to the
// release JSON and the tarball served under it.
type fakeRepo struct {
	ref, digest string
	releases    map[string]struct {
		rel *Release
		tgz []byte
	}
}

func fakeGitHub(t *testing.T, repo *fakeRepo) (*GitHub, *[]string) {
	t.Helper()
	var storageAuth []string
	var current []byte
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		w.Write(current)
	}))
	t.Cleanup(storage.Close)
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/loomux/server/contents/deploy/web-ref" && r.URL.Query().Get("ref") == "main":
			w.Write([]byte(repo.ref + "\n"))
		case r.URL.Path == "/repos/loomux/server/contents/deploy/web-sha256" && r.URL.Query().Get("ref") == "main":
			w.Write([]byte(repo.digest + "\n"))
		case strings.HasPrefix(r.URL.Path, "/repos/loomux/web/releases/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, "/repos/loomux/web/releases/tags/")
			rr, ok := repo.releases[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
				{"name": releaseFile, "url": api.URL + "/assets/" + tag + "/json"},
				{"name": rr.rel.Tarball, "url": api.URL + "/assets/" + tag + "/tgz"},
			}})
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			parts := strings.Split(r.URL.Path, "/")
			rr := repo.releases[parts[2]]
			if parts[3] == "json" {
				json.NewEncoder(w).Encode(rr.rel)
				return
			}
			current = rr.tgz
			http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	g := NewGitHub("loomux/server", "loomux/web", "tok")
	g.api = api.URL
	return g, &storageAuth
}

func (f *fakeRepo) publish(rel *Release, tgz []byte) {
	if f.releases == nil {
		f.releases = map[string]struct {
			rel *Release
			tgz []byte
		}{}
	}
	f.releases[rel.Tag] = struct {
		rel *Release
		tgz []byte
	}{rel, tgz}
}

// The release served is the one loomux/server main pins, checked against
// the pinned digest. A forged release — created by a workflow on some
// branch (so "github-actions[bot]"), for an ancestor commit of main, dated
// in the future, internally consistent — is never installed, because the
// pin doesn't name it (loomux/server#180 review).
func TestGitHubInstallsOnlyWhatThePinNames(t *testing.T) {
	ctx := context.Background()
	genuineTGZ := tarball(t, entry{name: "index.html", body: "genuine"})
	genuine := release("aaaaaaa", t0, genuineTGZ)
	forgedTGZ := tarball(t, entry{name: "index.html", body: "<script>steal()</script>"})
	forged := release("fffffff", t0.Add(100*365*24*time.Hour), forgedTGZ)
	repo := &fakeRepo{ref: genuine.Commit, digest: genuine.SHA256}
	repo.publish(genuine, genuineTGZ)
	repo.publish(forged, forgedTGZ)
	g, storageAuth := fakeGitHub(t, repo)

	rel, err := g.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Commit != genuine.Commit || rel.SHA256 != genuine.SHA256 {
		t.Fatalf("Latest = %s, want the pinned release", rel)
	}
	m, err := New(bakedDir(t, release("bbbbbbb", t0, nil)), t.TempDir(), g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if index(t, m) != "genuine" {
		t.Errorf("index %q, want the pinned bundle", index(t, m))
	}
	if len(*storageAuth) != 1 || (*storageAuth)[0] != "" {
		t.Errorf("storage saw Authorization %q, want it dropped on the off-host redirect", *storageAuth)
	}
}

// The pinned release's assets replaced after review — a new tarball, with
// or without its JSON rewritten to match — is refused: only the pinned
// digest counts.
func TestGitHubRefusesAReplacedRelease(t *testing.T) {
	ctx := context.Background()
	reviewedTGZ := tarball(t, entry{name: "index.html", body: "reviewed"})
	reviewed := release("aaaaaaa", t0, reviewedTGZ)
	evilTGZ := tarball(t, entry{name: "index.html", body: "evil"})

	t.Run("json rewritten to the new tarball", func(t *testing.T) {
		repo := &fakeRepo{ref: reviewed.Commit, digest: reviewed.SHA256}
		repo.publish(release("aaaaaaa", t0, evilTGZ), evilTGZ)
		g, _ := fakeGitHub(t, repo)
		if rel, err := g.Latest(ctx); err == nil {
			t.Errorf("Latest = %s, want it refused", rel)
		}
	})
	t.Run("tarball swapped under the reviewed json", func(t *testing.T) {
		repo := &fakeRepo{ref: reviewed.Commit, digest: reviewed.SHA256}
		repo.publish(reviewed, evilTGZ)
		g, _ := fakeGitHub(t, repo)
		m, err := New(bakedDir(t, nil), t.TempDir(), g)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Install(ctx); err == nil || !strings.Contains(err.Error(), "the pin says") {
			t.Errorf("Install = %v, want a digest mismatch", err)
		}
		if index(t, m) != "baked" {
			t.Errorf("index %q after a refused install", index(t, m))
		}
	})
}

func TestGitHubRefusesABadPin(t *testing.T) {
	tgz := tarball(t, entry{name: "index.html", body: "x"})
	rel := release("aaaaaaa", t0, tgz)
	for name, repo := range map[string]*fakeRepo{
		"short ref":      {ref: "aaaaaaa", digest: rel.SHA256},
		"bad digest":     {ref: rel.Commit, digest: "nope"},
		"no release":     {ref: strings.Repeat("c", 40), digest: rel.SHA256},
		"other repo":     {ref: rel.Commit, digest: rel.SHA256},
		"commit differs": {ref: rel.Commit, digest: rel.SHA256},
	} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "other repo":
				r := *rel
				r.Repo = "someone/else"
				repo.publish(&r, tgz)
			case "commit differs":
				r := *rel
				r.Commit = "aaaaaaa" + strings.Repeat("e", 33)
				repo.publish(&r, tgz)
			default:
				repo.publish(rel, tgz)
			}
			g, _ := fakeGitHub(t, repo)
			if got, err := g.Latest(context.Background()); err == nil {
				t.Errorf("Latest = %s, want it refused", got)
			}
		})
	}
}

func TestGitHubRefusesURLsOffTheAPIHost(t *testing.T) {
	g := NewGitHub("loomux/server", "loomux/web", "tok")
	if _, err := g.Tarball(context.Background(), &Release{tarballURL: "https://evil.example/x"}); err == nil {
		t.Error("fetched a tarball URL off the API host")
	}
}
