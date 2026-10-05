package webbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name, body string
	typ        byte
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: typ}
		if typ != tar.TypeReg {
			hdr.Size = 0
			hdr.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func release(short string, built time.Time, tgz []byte) *Release {
	sum := sha256.Sum256(tgz)
	commit := short + strings.Repeat("0", 40-len(short))
	return &Release{Schema: 1, Repo: "loomux/web", Commit: commit, Short: short, Tag: "web-" + short,
		Tarball: "loomux-web-" + short + ".tar.gz", SHA256: hex.EncodeToString(sum[:]), BuiltAt: built}
}

type fakeSource struct {
	rel  *Release
	tgz  []byte
	err  error
	gets int
}

func (f *fakeSource) Latest(context.Context) (*Release, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rel, nil
}

func (f *fakeSource) Tarball(context.Context, *Release) (io.ReadCloser, error) {
	f.gets++
	return io.NopCloser(bytes.NewReader(f.tgz)), nil
}

var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

// bakedDir is an image bundle built at t0 (no release file when rel is nil).
func bakedDir(t *testing.T, rel *Release) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("baked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		b, _ := json.Marshal(rel)
		if err := os.WriteFile(filepath.Join(dir, releaseFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func index(t *testing.T, m *Manager) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.Root(), "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallSwapsInANewerBundleAndRollsBack(t *testing.T) {
	ctx := context.Background()
	baked := release("aaaaaaa", t0, nil)
	tgz := tarball(t, entry{name: "./index.html", body: "new"}, entry{name: "./assets/app.js", body: "js"})
	src := &fakeSource{rel: release("bbbbbbb", t0.Add(time.Hour), tgz), tgz: tgz}
	dir := t.TempDir()
	m, err := New(bakedDir(t, baked), dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if latest, _ := m.Latest(ctx); !m.UpdateAvailable(latest) {
		t.Fatal("no update available for a newer release")
	}

	rel, err := m.Install(ctx)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rel.Short != "bbbbbbb" || index(t, m) != "new" || !m.Installed() || m.Previous().Short != "aaaaaaa" {
		t.Fatalf("after install: rel %v, index %q, installed %v, previous %v", rel, index(t, m), m.Installed(), m.Previous())
	}
	if _, err := m.Install(ctx); !errors.Is(err, ErrUpToDate) {
		t.Errorf("second Install = %v, want ErrUpToDate", err)
	}

	// A restart serves the installed bundle.
	m2, err := New(m.bakedDir, dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if index(t, m2) != "new" || m2.Previous() == nil {
		t.Errorf("after restart: index %q, previous %v", index(t, m2), m2.Previous())
	}

	if rel, err := m2.Rollback(); err != nil || rel.Short != "aaaaaaa" || index(t, m2) != "baked" {
		t.Fatalf("Rollback = %v, %v; index %q", rel, err, index(t, m2))
	}
	// Rolling back is remembered, and can be undone.
	m3, err := New(m.bakedDir, dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if index(t, m3) != "baked" {
		t.Errorf("after restart following rollback: index %q, want the image's", index(t, m3))
	}
	if _, err := m2.Rollback(); err != nil || index(t, m2) != "new" {
		t.Errorf("rolling forward again: %v, index %q", err, index(t, m2))
	}
}

func TestInstallRefusesAndKeepsServing(t *testing.T) {
	good := tarball(t, entry{name: "index.html", body: "new"})
	for _, tc := range []struct {
		name string
		tgz  []byte
		rel  func(*Release)
	}{
		{"sha256 mismatch", good, func(r *Release) { r.SHA256 = strings.Repeat("0", 64) }},
		{"no index.html", tarball(t, entry{name: "app.js", body: "js"}), nil},
		{"path escape", tarball(t, entry{name: "index.html", body: "x"}, entry{name: "../evil", body: "x"}), nil},
		{"absolute path", tarball(t, entry{name: "/etc/evil", body: "x"}), nil},
		{"symlink", tarball(t, entry{name: "index.html", body: "x"}, entry{name: "link", typ: tar.TypeSymlink}), nil},
		{"not gzip", []byte("not a tarball"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := release("bbbbbbb", t0.Add(time.Hour), tc.tgz)
			if tc.rel != nil {
				tc.rel(rel)
			}
			dir := t.TempDir()
			m, err := New(bakedDir(t, release("aaaaaaa", t0, nil)), dir, &fakeSource{rel: rel, tgz: tc.tgz})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Install(context.Background()); err == nil {
				t.Fatal("Install succeeded")
			}
			if index(t, m) != "baked" || m.Installed() || m.Previous() != nil {
				t.Errorf("after a refused install: index %q, installed %v", index(t, m), m.Installed())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
				t.Error("an entry was written outside the bundle")
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "bundles")); len(entries) != 0 {
				t.Errorf("bundles left behind: %v", entries)
			}
		})
	}
}

func TestInstallIgnoresAnOlderRelease(t *testing.T) {
	tgz := tarball(t, entry{name: "index.html", body: "old"})
	src := &fakeSource{rel: release("bbbbbbb", t0.Add(-time.Hour), tgz), tgz: tgz}
	m, err := New(bakedDir(t, release("aaaaaaa", t0, nil)), t.TempDir(), src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background()); !errors.Is(err, ErrUpToDate) || src.gets != 0 {
		t.Errorf("Install = %v (downloads %d), want ErrUpToDate without downloading", err, src.gets)
	}
}

// A newer image supersedes an older installed bundle.
func TestNewerImageWinsOverAnOlderInstall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tgz := tarball(t, entry{name: "index.html", body: "installed"})
	src := &fakeSource{rel: release("bbbbbbb", t0.Add(time.Hour), tgz), tgz: tgz}
	m, err := New(bakedDir(t, release("aaaaaaa", t0, nil)), dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}

	m2, err := New(bakedDir(t, release("ccccccc", t0.Add(2*time.Hour), nil)), dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if index(t, m2) != "baked" || m2.Installed() || m2.Current().Short != "ccccccc" {
		t.Errorf("index %q, installed %v, current %v; want the newer image's", index(t, m2), m2.Installed(), m2.Current())
	}
}

func TestDisabledWithoutSource(t *testing.T) {
	m, err := New(bakedDir(t, nil), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Enabled() || m.Current() != nil || index(t, m) != "baked" {
		t.Errorf("enabled %v, current %v", m.Enabled(), m.Current())
	}
	if _, err := m.Install(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("Install = %v, want ErrDisabled", err)
	}
	if _, err := m.Rollback(); !errors.Is(err, ErrNoPrevious) {
		t.Errorf("Rollback = %v, want ErrNoPrevious", err)
	}
}

// A corrupt state file or a missing bundle falls back to the image's.
func TestBrokenStateServesTheImage(t *testing.T) {
	for _, st := range []string{"{not json", `{"current":"bbbbbbb","previous":"baked"}`, `{"current":"../../etc"}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := New(bakedDir(t, nil), dir, nil)
		if err != nil {
			t.Fatalf("New with state %q: %v", st, err)
		}
		if index(t, m) != "baked" {
			t.Errorf("state %q: index %q, want the image's", st, index(t, m))
		}
	}
}
