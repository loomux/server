package webbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRelease struct {
	rel    *Release
	author string
	onMain bool
}

// fakeGitHub serves the releases API for loomux/web; tarballs redirect to
// a separate storage host, as GitHub's do.
func fakeGitHub(t *testing.T, tgz []byte, releases ...fakeRelease) (*GitHub, *[]string) {
	t.Helper()
	var storageAuth []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		w.Write(tgz)
	}))
	t.Cleanup(storage.Close)
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/loomux/web/releases":
			var list []map[string]any
			for _, fr := range releases {
				list = append(list, map[string]any{
					"tag_name": fr.rel.Tag, "prerelease": true, "author": map[string]string{"login": fr.author},
					"assets": []map[string]string{
						{"name": releaseFile, "url": api.URL + "/assets/" + fr.rel.Short + "/json"},
						{"name": fr.rel.Tarball, "url": api.URL + "/assets/" + fr.rel.Short + "/tgz"},
					},
				})
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			parts := strings.Split(r.URL.Path, "/")
			for _, fr := range releases {
				if fr.rel.Short != parts[2] {
					continue
				}
				if parts[3] == "json" {
					json.NewEncoder(w).Encode(fr.rel)
				} else {
					http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
				}
				return
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/repos/loomux/web/compare/"):
			status := "diverged"
			for _, fr := range releases {
				if strings.Contains(r.URL.Path, fr.rel.Commit) && fr.onMain {
					status = "ahead"
				}
			}
			fmt.Fprintf(w, `{"status":%q}`, status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	g := NewGitHub("loomux/web", "tok")
	g.api = api.URL
	return g, &storageAuth
}

func TestGitHubLatestTrustsOnlyCIReleasesOnMain(t *testing.T) {
	tgz := tarball(t, entry{name: "index.html", body: "from github"})
	genuine := release("aaaaaaa", t0, tgz)
	uploaded := release("bbbbbbb", t0.Add(time.Hour), tgz)
	offMain := release("ccccccc", t0.Add(2*time.Hour), tgz)
	g, storageAuth := fakeGitHub(t, tgz,
		fakeRelease{offMain, ciAuthor, false},
		fakeRelease{uploaded, "somebody", true},
		fakeRelease{genuine, ciAuthor, true},
	)

	rel, err := g.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Short != "aaaaaaa" {
		t.Fatalf("Latest = %s, want the newest release CI published from main", rel)
	}

	// Installing it follows the redirect to storage without the token.
	m, err := New(bakedDir(t, nil), t.TempDir(), g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if index(t, m) != "from github" {
		t.Errorf("index %q", index(t, m))
	}
	if len(*storageAuth) != 1 || (*storageAuth)[0] != "" {
		t.Errorf("storage saw Authorization %q, want it dropped on the off-host redirect", *storageAuth)
	}
}

func TestGitHubLatestRejectsInconsistentRelease(t *testing.T) {
	tgz := tarball(t, entry{name: "index.html", body: "x"})
	for name, mutate := range map[string]func(*Release){
		"tag disagrees":  func(r *Release) { r.Tag = "web-ddddddd" },
		"other repo":     func(r *Release) { r.Repo = "someone/else" },
		"tarball name":   func(r *Release) { r.Tarball = "../../x.tar.gz" },
		"bad sha256":     func(r *Release) { r.SHA256 = "nope" },
		"commit/short":   func(r *Release) { r.Commit = strings.Repeat("e", 40) },
		"no build stamp": func(r *Release) { r.BuiltAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			rel := release("aaaaaaa", t0, tgz)
			served := *rel
			mutate(&served)
			g, _ := fakeGitHub(t, tgz, fakeRelease{&served, ciAuthor, true})
			if got, err := g.Latest(context.Background()); err == nil {
				t.Errorf("Latest = %s, want it refused", got)
			}
		})
	}
}

func TestGitHubRefusesURLsOffTheAPIHost(t *testing.T) {
	g := NewGitHub("loomux/web", "tok")
	if _, err := g.Tarball(context.Background(), &Release{tarballURL: "https://evil.example/x"}); err == nil {
		t.Error("fetched a tarball URL off the API host")
	}
}
