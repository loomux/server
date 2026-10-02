package router

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VersionCheck declares a known-good version range for an agent-type's
// underlying CLI, checked before every launch (design spec §10 axis 3:
// "declares a known-good version range for the underlying tool, checked
// at launch"). Command is run via targets.TargetExecutor.RunOnce; Parse
// extracts a comparable dotted-number version string from its raw
// output — each agent-type's own CLI has its own format (e.g. `claude
// --version` prints "2.1.251 (Claude Code)", not strict semver), so
// Parse is per-adapter rather than one shared regex trying to fit every
// tool. Min/Max bound the extracted version — see CheckVersionRange.
type VersionCheck struct {
	Command string
	Parse   func(output string) (string, error)
	// Min is the inclusive lower bound; empty means no lower bound.
	Min string
	// Max is the exclusive upper bound; empty means no upper bound.
	Max string
	// Requires names what Min exists for (e.g. "completion hooks
	// (--settings)"), so a too-old CLI fails with "agent version too old
	// for <Requires>" rather than a bare range error (LOOM-75). Optional.
	Requires string
}

// ErrVersionTooOld is wrapped by CheckVersionRange's below-minimum
// error, so callers can tell "too old" from "too new" or "unparseable".
var ErrVersionTooOld = errors.New("agent version below the supported minimum")

// ExtractDottedVersion is a reusable VersionCheck.Parse: it finds the
// first dotted-number sequence in output (e.g. "2.1.251" out of
// "2.1.251 (Claude Code)") via regex. Agent-type adapters whose CLI's
// --version output fits this common shape can use this directly;
// others may need their own Parse.
func ExtractDottedVersion(output string) (string, error) {
	m := dottedVersionPattern.FindString(output)
	if m == "" {
		return "", fmt.Errorf("no dotted-number version found in %q", output)
	}
	return m, nil
}

var dottedVersionPattern = regexp.MustCompile(`\d+(\.\d+)+`)

// CheckVersionRange checks that version falls within [min, max) — min
// inclusive, max exclusive, matching how most "known-good range"
// requirements are phrased ("at least X, before the Y that broke
// things"). Either bound may be empty for "no bound".
func CheckVersionRange(version, min, max string) error {
	if min != "" {
		cmp, err := compareVersions(version, min)
		if err != nil {
			return err
		}
		if cmp < 0 {
			return fmt.Errorf("version %s is below the minimum %s: %w", version, min, ErrVersionTooOld)
		}
	}
	if max != "" {
		cmp, err := compareVersions(version, max)
		if err != nil {
			return err
		}
		if cmp >= 0 {
			return fmt.Errorf("version %s is at or above the maximum %s", version, max)
		}
	}
	return nil
}

// compareVersions compares two dotted-number version strings (e.g.
// "2.1.251") component-wise — not full semver (no pre-release/build
// metadata handling), sufficient for the simple dotted-number CLI
// version outputs VersionCheck deals with. Missing trailing components
// are treated as 0, so "2.1" == "2.1.0". Returns -1, 0, or 1.
func compareVersions(a, b string) (int, error) {
	av, err := parseDottedVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := parseDottedVersion(b)
	if err != nil {
		return 0, err
	}

	n := len(av)
	if len(bv) > n {
		n = len(bv)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			if x < y {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parseDottedVersion(s string) ([]int, error) {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid version component %q in %q: %w", p, s, err)
		}
		out[i] = n
	}
	return out, nil
}
