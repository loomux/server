package webbundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeWebRepo serves loomux/web's releases (newest first), their assets,
// attestations by digest and a compare endpoint (commit order).
type fakeWebRepo struct {
	releases []*Release
	tgz      map[string][]byte // by short
	attested map[string]bool   // sha256 -> has a (good) attestation
	order    []string          // commits, oldest first, on main
}

func (f *fakeWebRepo) serve(t *testing.T) *Attested {
	t.Helper()
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/repos/loomux/web/releases":
			var list []map[string]any
			for _, rel := range f.releases {
				list = append(list, map[string]any{"tag_name": rel.Tag, "published_at": rel.BuiltAt, "assets": []map[string]string{
					{"name": releaseFile, "url": api.URL + "/assets/" + rel.Short + "/json"},
					{"name": rel.Tarball, "url": api.URL + "/assets/" + rel.Short + "/tgz"},
				}})
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(p, "/assets/"):
			parts := strings.Split(p, "/")
			for _, rel := range f.releases {
				if rel.Short == parts[2] {
					if parts[3] == "json" {
						json.NewEncoder(w).Encode(rel)
					} else {
						w.Write(f.tgz[rel.Short])
					}
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(p, "/repos/loomux/web/attestations/sha256:"):
			digest := strings.TrimPrefix(p, "/repos/loomux/web/attestations/sha256:")
			var atts []map[string]any
			if f.attested[digest] {
				atts = append(atts, map[string]any{"bundle": map[string]string{"digest": digest}})
			}
			json.NewEncoder(w).Encode(map[string]any{"attestations": atts})
		case strings.HasPrefix(p, "/repos/loomux/web/compare/"):
			base, head, _ := strings.Cut(strings.TrimPrefix(p, "/repos/loomux/web/compare/"), "...")
			idx := func(c string) int {
				for i, o := range f.order {
					if o == c {
						return i
					}
				}
				return -1
			}
			status := "diverged"
			switch b, h := idx(base), idx(head); {
			case b < 0 || h < 0:
			case h > b:
				status = "ahead"
			case h < b:
				status = "behind"
			default:
				status = "identical"
			}
			fmt.Fprintf(w, `{"status":%q}`, status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	a := NewAttested("loomux/web", "", t.TempDir())
	a.gh.api = api.URL
	// The bundle's own cryptography is TestVerifyAttestation's; here a
	// "good" bundle is one the fake API served for the release's digest.
	a.checkBundle = func(raw json.RawMessage, rel *Release) error {
		var b struct{ Digest string }
		json.Unmarshal(raw, &b)
		if b.Digest != rel.SHA256 {
			return errors.New("bundle is for another digest")
		}
		return nil
	}
	return a
}

func (f *fakeWebRepo) add(t *testing.T, short string, built time.Time, attested bool) *Release {
	t.Helper()
	tgz := tarball(t, entry{name: "index.html", body: short})
	rel := release(short, built, tgz)
	if f.tgz == nil {
		f.tgz, f.attested = map[string][]byte{}, map[string]bool{}
	}
	f.tgz[short] = tgz
	f.attested[rel.SHA256] = attested
	f.releases = append([]*Release{rel}, f.releases...) // newest first
	f.order = append(f.order, rel.Commit)
	return rel
}

// The newest release with a verified attestation is offered; one
// without (a branch's release, say) is skipped.
func TestAttestedLatestSkipsUnattested(t *testing.T) {
	f := &fakeWebRepo{}
	f.add(t, "aaaaaaa", t0, true)
	good := f.add(t, "bbbbbbb", t0.Add(time.Hour), true)
	f.add(t, "ccccccc", t0.Add(2*time.Hour), false)
	a := f.serve(t)
	rel, err := a.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Commit != good.Commit {
		t.Errorf("Latest = %s, want the newest attested (%s)", rel, good)
	}
}

// An update is only ever forward on main: the Manager installs the
// attested release ahead of what's served, and never steps back to an
// older one (after a rollback the newer one is offered again).
func TestAttestedInstallsOnlyForward(t *testing.T) {
	ctx := context.Background()
	f := &fakeWebRepo{}
	older := f.add(t, "aaaaaaa", t0, true)
	newer := f.add(t, "bbbbbbb", t0.Add(time.Hour), true)
	a := f.serve(t)

	// Served: the newer commit (as an image bundle); the attested latest
	// is that same commit: nothing to do.
	m, err := New(bakedDir(t, newer), t.TempDir(), a)
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := m.Latest(ctx); err != nil || m.UpdateAvailable(latest) {
		t.Fatalf("latest %v, %v: an update offered for the commit served", latest, err)
	}
	if _, err := m.Install(ctx); !errors.Is(err, ErrUpToDate) {
		t.Errorf("Install = %v, want ErrUpToDate", err)
	}

	// Served: older; newer is ahead: offered and installed.
	m2, err := New(bakedDir(t, older), t.TempDir(), a)
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := m2.Latest(ctx); err != nil || !m2.UpdateAvailable(latest) {
		t.Fatalf("latest %v, %v: no update offered for a newer attested release", latest, err)
	}
	if rel, err := m2.Install(ctx); err != nil || rel.Commit != newer.Commit || index(t, m2) != "bbbbbbb" {
		t.Fatalf("Install = %v, %v; index %q", rel, err, index(t, m2))
	}

	// The newest release now is behind what's served (history rewound,
	// or an old release re-published): not an update.
	f.releases = []*Release{older}
	m3, err := New(bakedDir(t, newer), t.TempDir(), a)
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := m3.Latest(ctx); err != nil || m3.UpdateAvailable(latest) {
		t.Errorf("latest %v, %v: an older release offered as an update", latest, err)
	}
	if _, err := m3.Install(ctx); !errors.Is(err, ErrUpToDate) {
		t.Errorf("Install of an older release = %v, want ErrUpToDate", err)
	}
}
