// Package version holds loomuxd's own release version (design spec §10
// axis 4) — independent of the API version (axis 1, /api/v1/...): a
// server patch release doesn't force an API version bump, only a
// breaking API change does.
package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is loomuxd's semver release version. Overridable at build
// time via
//
//	go build -ldflags "-X github.com/Loomux/server/version.Version=1.2.3"
//
// "dev" when built without that flag (e.g. a local `go build`/`go run`).
// CI stamps "<release> (web <web version>)" or, unreleased, "<short
// sha> (web ...)"; Parse reads the leading token.
var Version = "dev"

// Semver is a parsed X.Y.Z[-pre] version (SemVer 2.0.0, no build
// metadata), as the release line and plugin manifests use it
// (LOOM-194).
type Semver struct {
	Major, Minor, Patch int
	// Pre is the pre-release part after the hyphen ("rc.1"), "" for a
	// release.
	Pre string
}

var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// Parse reads the version a version string starts with: "0.3.74 (web
// 0.3.12)" is 0.3.74. "dev", a commit, or anything that isn't X.Y.Z
// is not a version (ok false): a host built without a release.
func Parse(s string) (Semver, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return Semver{}, false
	}
	m := semver.FindStringSubmatch(fields[0])
	if m == nil {
		return Semver{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Semver{Major: major, Minor: minor, Patch: patch, Pre: m[4]}, true
}

// String is X.Y.Z or X.Y.Z-pre.
func (v Semver) String() string {
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Compare orders versions: -1, 0 or 1 as v is before, the same as or
// after o. A pre-release comes before its release; two pre-releases
// compare as strings.
func (v Semver) Compare(o Semver) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	return strings.Compare(v.Pre, o.Pre)
}

// Satisfies reports whether v meets a minimum the way a caret range
// does: at or above min, in the same major line, and under 0.x in the
// same minor line (0.Y is the major there). A plugin's
// min_host_version 0.3.0 is met by 0.3.80, not by 0.4.0.
func (v Semver) Satisfies(min Semver) bool {
	if v.Major != min.Major || (v.Major == 0 && v.Minor != min.Minor) {
		return false
	}
	return v.Compare(min) >= 0
}
