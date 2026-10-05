package version_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deploy/release.sh is what the image workflow trusts to read a tag
// (LOOM-129): exercise it here, since a release tag is pushed only
// once.
func release(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	out, err := exec.Command("sh", append([]string{"../deploy/release.sh"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

func TestReleaseVersion(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.1.0": "0.1.0", "v1.0.0-rc.1": "1.0.0-rc.1", "v1.2.3-alpha.1.x-y": "1.2.3-alpha.1.x-y", "v10.20.30": "10.20.30",
	} {
		if got, ok := release(t, "version", tag); !ok || got != want {
			t.Errorf("version %s = %q, %v; want %q", tag, got, ok, want)
		}
	}
	for _, tag := range []string{"0.1.0", "v1.0", "v01.0.0", "v1.0.0+build.1", "v1.0.0-01", "v1.0.0-", "vx.y.z", "v1.0.0 ; rm"} {
		if got, ok := release(t, "version", tag); ok {
			t.Errorf("version %s accepted as %q", tag, got)
		}
	}
}

func TestReleasePrerelease(t *testing.T) {
	for v, want := range map[string]bool{"0.1.0": true, "0.9.3": true, "1.0.0-rc.1": true, "1.0.0": false, "2.3.4": false} {
		if _, ok := release(t, "prerelease", v); ok != want {
			t.Errorf("prerelease %s = %v, want %v", v, ok, want)
		}
	}
}

func TestReleaseNotes(t *testing.T) {
	changelog := filepath.Join(t.TempDir(), "CHANGELOG.md")
	os.WriteFile(changelog, []byte(`# Changelog

## [Unreleased]

- next thing

## [0.2.0] - 2026-10-06

### Added
- second

## [0.1.0] - 2026-10-05

### Added
- first

[0.2.0]: https://example.com
`), 0o644)
	if got, ok := release(t, "notes", "0.2.0", changelog); !ok || got != "### Added\n- second" {
		t.Errorf("notes 0.2.0 = %q, %v", got, ok)
	}
	if got, ok := release(t, "notes", "0.1.0", changelog); !ok || got != "### Added\n- first" {
		t.Errorf("notes 0.1.0 = %q, %v", got, ok)
	}
	if _, ok := release(t, "notes", "0.3.0", changelog); ok {
		t.Error("a version without a CHANGELOG section was accepted")
	}
	if _, ok := release(t, "notes", "0.1", changelog); ok {
		t.Error("a prefix of a version matched its section")
	}
}
