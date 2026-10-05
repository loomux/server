package webbundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source lists published web bundles and fetches their tarballs.
type Source interface {
	// Latest is the release to serve, already validated against its pin.
	Latest(ctx context.Context) (*Release, error)
	// Tarball opens rel's tarball (as returned by Latest).
	Tarball(ctx context.Context, rel *Release) (io.ReadCloser, error)
}

var (
	// ErrDisabled: no Source is configured, so nothing can be installed.
	ErrDisabled = errors.New("webbundle: updates are not configured")
	// ErrUpToDate: the pinned release is already served.
	ErrUpToDate = errors.New("webbundle: already up to date")
	// ErrNoPrevious: there is nothing to roll back to.
	ErrNoPrevious = errors.New("webbundle: no previous bundle to roll back to")
)

// Limits on what an install unpacks, far above a real bundle (~1 MB).
const (
	maxTarballBytes  = 64 << 20
	maxUnpackedBytes = 256 << 20
	maxFiles         = 5000
)

// baked names the image's own bundle in the state file.
const baked = "baked"

// state is what dir/state.json records: the bundle served and the one it
// replaced, each a release's short sha, baked, or "" for none; and Image,
// the commit of the image's own bundle when they were chosen, so a newly
// deployed image is noticed.
type state struct {
	Current  string `json:"current"`
	Previous string `json:"previous"`
	Image    string `json:"image"`
}

// served is the bundle being served.
type served struct {
	root string
	rel  *Release // nil for an image bundle without a web-release.json
	name string   // its state name: baked or a short sha
}

// Manager serves the newest of the image's bundle and an installed one,
// and installs and rolls back bundles. Safe for concurrent use; installs
// and rollbacks run one at a time.
type Manager struct {
	bakedDir string
	bakedRel *Release
	dir      string
	source   Source

	mu      sync.Mutex // serializes Install and Rollback
	current atomic.Pointer[served]
	prev    atomic.Pointer[served]

	latestMu  sync.Mutex
	latest    *Release
	latestAt  time.Time
	latestTTL time.Duration
}

// New opens the bundles in dir (created if missing) next to the image's
// bundle in bakedDir. source may be nil: the Manager then serves what it
// has but can't install. An installed bundle is served only while the
// image is the one it was installed over: deploying another image means
// starting from its bundle.
func New(bakedDir, dir string, source Source) (*Manager, error) {
	bakedRel, err := readRelease(bakedDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{bakedDir: bakedDir, bakedRel: bakedRel, dir: dir, source: source, latestTTL: time.Minute}
	if err := os.MkdirAll(filepath.Join(dir, "bundles"), 0o755); err != nil {
		return nil, fmt.Errorf("webbundle: %w", err)
	}
	st, err := m.readState()
	if err != nil {
		return nil, err
	}
	image := &served{root: bakedDir, rel: bakedRel, name: baked}
	cur := image
	if s := m.open(st.Current); s != nil && s.name != baked && st.Image == bakedRel.commit() {
		cur = s
		if p := m.open(st.Previous); p != nil {
			m.prev.Store(p)
		}
	}
	m.current.Store(cur)
	return m, nil
}

// Root is the directory to serve the web client from right now.
func (m *Manager) Root() string { return m.current.Load().root }

// Current is the release being served; nil when it's an image bundle
// that doesn't say what it is.
func (m *Manager) Current() *Release { return m.current.Load().rel }

// Installed reports whether the bundle served was installed (true) or
// came with the image (false).
func (m *Manager) Installed() bool { return m.current.Load().name != baked }

// Previous is the release Rollback would return to, or nil.
func (m *Manager) Previous() *Release {
	if p := m.prev.Load(); p != nil {
		return p.rel
	}
	return nil
}

// Enabled reports whether a Source is configured.
func (m *Manager) Enabled() bool { return m.source != nil }

// Latest is the release loomux/server main pins, cached for a minute.
func (m *Manager) Latest(ctx context.Context) (*Release, error) {
	if m.source == nil {
		return nil, ErrDisabled
	}
	m.latestMu.Lock()
	defer m.latestMu.Unlock()
	if m.latest != nil && time.Since(m.latestAt) < m.latestTTL {
		return m.latest, nil
	}
	rel, err := m.source.Latest(ctx)
	if err != nil {
		return nil, err
	}
	m.latest, m.latestAt = rel, time.Now()
	return rel, nil
}

// UpdateAvailable reports whether latest (the pinned release) isn't the
// bundle served. Pinning an older commit is a change too: the pin, not a
// timestamp, says what to serve.
func (m *Manager) UpdateAvailable(latest *Release) bool {
	return latest != nil && latest.Commit != m.Current().commit()
}

// Install fetches the pinned release and, unless it's already served,
// verifies and unpacks it and switches to it; the bundle it replaces
// becomes the one Rollback returns to. Anything that fails leaves the
// bundle served as it was.
func (m *Manager) Install(ctx context.Context) (*Release, error) {
	if m.source == nil {
		return nil, ErrDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rel, err := m.source.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if !m.UpdateAvailable(rel) {
		return nil, ErrUpToDate
	}
	bundle := filepath.Join(m.dir, "bundles", rel.Short)
	if err := m.unpack(ctx, rel, bundle); err != nil {
		return nil, err
	}
	cur := m.current.Load()
	next := &served{root: bundle, rel: rel, name: rel.Short}
	if err := m.writeState(state{Current: next.name, Previous: cur.name, Image: m.bakedRel.commit()}); err != nil {
		return nil, err
	}
	m.prev.Store(cur)
	m.current.Store(next)
	m.prune()
	return rel, nil
}

// Rollback switches back to the bundle the last Install replaced; the one
// served becomes the bundle to roll forward to.
func (m *Manager) Rollback() (*Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.prev.Load()
	if prev == nil {
		return nil, ErrNoPrevious
	}
	cur := m.current.Load()
	if err := m.writeState(state{Current: prev.name, Previous: cur.name, Image: m.bakedRel.commit()}); err != nil {
		return nil, err
	}
	m.current.Store(prev)
	m.prev.Store(cur)
	return prev.rel, nil
}

// open is the bundle a state name refers to, or nil if it's gone or
// broken.
func (m *Manager) open(name string) *served {
	switch name {
	case "":
		return nil
	case baked:
		return &served{root: m.bakedDir, rel: m.bakedRel, name: baked}
	}
	if !sha7(name) {
		return nil
	}
	root := filepath.Join(m.dir, "bundles", name)
	rel, err := readRelease(root)
	if err != nil || rel == nil || rel.Short != name {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		return nil
	}
	return &served{root: root, rel: rel, name: name}
}

func sha7(s string) bool {
	return len(s) == 7 && strings.Trim(s, "0123456789abcdef") == ""
}

// unpack downloads rel's tarball, checks its sha256, and unpacks it into
// bundle, replacing what's there: into a temporary directory first,
// renamed into place only once it's whole and has an index.html.
func (m *Manager) unpack(ctx context.Context, rel *Release, bundle string) error {
	tmpDir, err := os.MkdirTemp(m.dir, ".install-")
	if err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tarball := filepath.Join(tmpDir, rel.Tarball)
	if err := m.download(ctx, rel, tarball); err != nil {
		return err
	}
	root := filepath.Join(tmpDir, "bundle")
	if err := extract(tarball, root); err != nil {
		return fmt.Errorf("webbundle: unpack %s: %w", rel.Tag, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "index.html")); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("webbundle: %s has no index.html", rel.Tag)
	}
	b, err := json.MarshalIndent(rel, "", "  ")
	if err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, releaseFile), b, 0o644); err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	if err := os.RemoveAll(bundle); err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	if err := os.Rename(root, bundle); err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	return nil
}

// download writes rel's tarball to dst, refusing one that doesn't hash
// to rel.SHA256.
func (m *Manager) download(ctx context.Context, rel *Release, dst string) error {
	body, err := m.source.Tarball(ctx, rel)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxTarballBytes+1))
	if err != nil {
		return fmt.Errorf("webbundle: download %s: %w", rel.Tag, err)
	}
	if n > maxTarballBytes {
		return fmt.Errorf("webbundle: %s is larger than %d bytes", rel.Tarball, maxTarballBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rel.SHA256 {
		return fmt.Errorf("webbundle: %s has sha256 %s, the pin says %s", rel.Tarball, got, rel.SHA256)
	}
	return f.Close()
}

// extract unpacks a gzipped tar of regular files and directories into
// root, refusing anything else: links, devices, absolute paths, paths
// leaving root, and archives past the size and file-count limits.
func extract(tarball, root string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.Mkdir(root, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for files := 0; ; files++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if files >= maxFiles {
			return fmt.Errorf("more than %d entries", maxFiles)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(hdr.Name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("entry %q leaves the bundle", hdr.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxUnpackedBytes {
				return fmt.Errorf("unpacks to more than %d bytes", maxUnpackedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := writeFile(dst, tr, hdr.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %q is not a regular file or directory", hdr.Name)
		}
	}
}

func writeFile(dst string, r io.Reader, size int64) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, r, size); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (m *Manager) readState() (state, error) {
	var st state
	b, err := os.ReadFile(filepath.Join(m.dir, "state.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("webbundle: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		// A torn or hand-edited file: serve the image's bundle rather than
		// refuse to start.
		return state{}, nil
	}
	return st, nil
}

// writeState replaces state.json atomically: a crash leaves the old one
// or the new one, never half of either.
func (m *Manager) writeState(st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := filepath.Join(m.dir, ".state-"+hex.EncodeToString(suffix[:]))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("webbundle: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, "state.json")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("webbundle: %w", err)
	}
	return nil
}

// prune removes installed bundles that are neither served nor the one to
// roll back to. Best effort: a bundle left behind costs only disk.
func (m *Manager) prune() {
	keep := map[string]bool{m.current.Load().name: true}
	if p := m.prev.Load(); p != nil {
		keep[p.name] = true
	}
	entries, err := os.ReadDir(filepath.Join(m.dir, "bundles"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(m.dir, "bundles", e.Name()))
		}
	}
}
