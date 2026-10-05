#!/bin/sh
# Release helpers for the image workflow (LOOM-129; docs/release/versioning.md).
#
#   deploy/release.sh version <tag>            print the version a tag names
#   deploy/release.sh prerelease <version>     exit 0 if it's a pre-release
#   deploy/release.sh notes <version> [file]   print its CHANGELOG section
#
# A tag is "v" + a SemVer 2.0.0 version without build metadata. Every
# 0.y.z version and every version with a pre-release part (-alpha.1,
# -rc.2, ...) is a pre-release. A release's notes are its section of
# CHANGELOG.md ("## [X.Y.Z] - YYYY-MM-DD"); a tag without one is refused.
set -eu

semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'

die() { echo "release: $*" >&2; exit 1; }

case "${1:-}" in
version)
  tag="${2:-}"
  case "$tag" in v*) ;; *) die "tag '$tag' doesn't start with v" ;; esac
  v="${tag#v}"
  printf '%s\n' "$v" | grep -Eq "$semver" || die "tag '$tag' is not v + a SemVer 2.0.0 version (no build metadata)"
  printf '%s\n' "$v"
  ;;
prerelease)
  v="${2:-}"
  case "$v" in 0.* | *-*) exit 0 ;; *) exit 1 ;; esac
  ;;
notes)
  v="${2:-}"
  file="${3:-CHANGELOG.md}"
  [ -f "$file" ] || die "no $file"
  notes="$(mktemp)"
  trap 'rm -f "$notes"' EXIT
  # The section runs to the next version heading or the link definitions
  # ("[0.1.0]: https://...") that end the file.
  awk -v v="$v" '
    /^## \[/ { if (on) exit; on = (index($0, "## [" v "] - ") == 1); next }
    /^\[[^]]+\]: / { if (on) exit }
    on { print }
  ' "$file" > "$notes"
  grep -q '[^[:space:]]' "$notes" || die "$file has no non-empty section '## [$v] - <date>'"
  cat "$notes"
  ;;
*)
  die "usage: $0 version <tag> | prerelease <version> | notes <version> [file]"
  ;;
esac
