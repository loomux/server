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

func TestReleaseNext(t *testing.T) {
	for args, want := range map[[2]string]string{
		{"patch", "0.1.0"}: "0.1.1", {"patch", "0.1.9"}: "0.1.10", {"minor", "0.1.7"}: "0.2.0", {"minor", "1.9.3"}: "1.10.0",
	} {
		if got, ok := release(t, "next", args[0], args[1]); !ok || got != want {
			t.Errorf("next %v = %q, %v; want %q", args, got, ok, want)
		}
	}
	for _, args := range [][2]string{{"patch", "0.1"}, {"major", "0.1.0"}, {"patch", "1.0.0-rc.1"}} {
		if got, ok := release(t, "next", args[0], args[1]); ok {
			t.Errorf("next %v accepted as %q", args, got)
		}
	}
}

// The line continues from the highest plain version reachable from the
// commit: not a -rc, not a tag on another branch, and compared as
// numbers (0.1.10 > 0.1.9).
func TestReleaseLatest(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	latest := func() string {
		t.Helper()
		cmd := exec.Command("sh", filepath.Join(must(os.Getwd()), "../deploy/release.sh"), "latest")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("latest: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "a")
	if got := latest(); got != "" {
		t.Errorf("latest with no tags = %q", got)
	}
	git("tag", "v0.1.0")
	git("commit", "-q", "--allow-empty", "-m", "b")
	git("tag", "v0.1.9")
	git("commit", "-q", "--allow-empty", "-m", "c")
	git("tag", "v0.1.10")
	git("tag", "v1.0.0-rc.1")
	git("tag", "not-a-version")
	git("checkout", "-q", "-b", "side", "HEAD~2")
	git("commit", "-q", "--allow-empty", "-m", "side")
	git("tag", "v0.5.0")
	git("checkout", "-q", "main")
	if got := latest(); got != "0.1.10" {
		t.Errorf("latest = %q, want 0.1.10", got)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

// LOOM-129 review: two merges racing for the next version both get one.
// The fake gh refuses the first tag as if another run had just created
// it (and that run's tag appears, as a fetch would bring it); reserve
// takes the next.
func TestReleaseReserve(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("tag", "v0.1.0")
	git("commit", "-q", "--allow-empty", "-m", "other merge")
	other := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "this merge")
	sha := git("rev-parse", "HEAD")

	bin := t.TempDir()
	created := filepath.Join(bin, "created")
	fake := `#!/bin/sh
case "$*" in
*refs/tags/v0.1.1*) git -C "` + dir + `" tag v0.1.1 ` + other + `; echo 'gh: Reference already exists (HTTP 422)' >&2; exit 1 ;;
*) echo "$*" >> "` + created + `"; exit 0 ;;
esac
`
	os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755)
	script := filepath.Join(must(os.Getwd()), "../deploy/release.sh")
	reserve := func(sha, kind string) (string, bool) {
		cmd := exec.Command("sh", script, "reserve", sha, kind)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_REPOSITORY=loomux/x")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err == nil
	}
	if got, ok := reserve(sha, "patch"); !ok || got != "0.1.2" {
		t.Fatalf("reserve = %q, %v; want 0.1.2 after losing 0.1.1", got, ok)
	}
	if b, _ := os.ReadFile(created); !strings.Contains(string(b), "ref=refs/tags/v0.1.2") || !strings.Contains(string(b), "sha="+sha) {
		t.Errorf("gh calls = %q", b)
	}
	// Asked again for the same commit (a re-run), it keeps its version.
	if got, ok := reserve(sha, "patch"); !ok || got != "0.1.2" {
		t.Errorf("re-run reserve = %q, %v; want 0.1.2 again", got, ok)
	}
}
