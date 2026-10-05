// Package webbundle keeps the web client loomuxd serves current without
// rebuilding its image (LOOM-118). loomux/web's CI publishes every tested
// main commit as a pre-release web-<short sha> (LOOM-58); a Manager lists
// them, installs a newer one — downloaded, checked against its sha256,
// unpacked, then swapped in atomically — and can roll back to the bundle
// it replaced. The image's own bundle is the floor it never deletes.
package webbundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Release is a published web bundle: the web-release.json loomux/web's
// CI writes next to the tarball (loomux/web docs/release.md).
type Release struct {
	Schema  int       `json:"schema"`
	Repo    string    `json:"repo"`
	Commit  string    `json:"commit"`
	Short   string    `json:"short"`
	Tag     string    `json:"tag"`
	Tarball string    `json:"tarball"`
	SHA256  string    `json:"sha256"`
	BuiltAt time.Time `json:"built_at"`
	Subject string    `json:"subject,omitempty"`

	// tarballURL is where a Source fetches the tarball from; not part of
	// the published JSON.
	tarballURL string
}

// releaseFile is a bundle's own copy of its Release, in its directory.
const releaseFile = "web-release.json"

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate checks r is self-consistent: tag, short sha, commit and
// tarball name all agree, and the digest is well formed. It says nothing
// about whether r is genuine — that is the Source's job.
func (r *Release) Validate() error {
	switch {
	case r.Schema != 1:
		return fmt.Errorf("webbundle: release schema %d, want 1", r.Schema)
	case !commitRE.MatchString(r.Commit):
		return fmt.Errorf("webbundle: release commit %q is not a full sha", r.Commit)
	case r.Short != r.Commit[:7]:
		return fmt.Errorf("webbundle: release short %q doesn't match commit %s", r.Short, r.Commit)
	case r.Tag != "web-"+r.Short:
		return fmt.Errorf("webbundle: release tag %q, want web-%s", r.Tag, r.Short)
	case r.Tarball != "loomux-web-"+r.Short+".tar.gz":
		return fmt.Errorf("webbundle: release tarball %q, want loomux-web-%s.tar.gz", r.Tarball, r.Short)
	case !sha256RE.MatchString(r.SHA256):
		return fmt.Errorf("webbundle: release sha256 %q is not 64 hex characters", r.SHA256)
	case r.BuiltAt.IsZero():
		return fmt.Errorf("webbundle: release %s has no built_at", r.Tag)
	}
	return nil
}

// readRelease reads the Release a bundle directory holds, or nil when it
// has none (an image built before LOOM-118 baked one in).
func readRelease(dir string) (*Release, error) {
	b, err := os.ReadFile(filepath.Join(dir, releaseFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webbundle: %w", err)
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("webbundle: %s: %w", filepath.Join(dir, releaseFile), err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// newer reports whether r was built later than than (nil: anything is).
func (r *Release) newer(than *Release) bool {
	return than == nil || r.BuiltAt.After(than.BuiltAt)
}

// String is the release's tag, for logs and errors.
func (r *Release) String() string {
	if r == nil {
		return "<none>"
	}
	return strings.TrimSpace(r.Tag)
}
