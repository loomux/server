#!/bin/sh
# Release helpers for the image workflow (LOOM-129; docs/release/versioning.md).
#
#   deploy/release.sh version <tag>            print the version a tag names
#   deploy/release.sh prerelease <version>     exit 0 if it's a pre-release
#   deploy/release.sh notes <version> [file]   print its CHANGELOG section
#   deploy/release.sh latest [rev|--all]       the highest vX.Y.Z tag reachable
#                                              from rev (HEAD), or of all tags
#   deploy/release.sh reserve <sha> <patch|minor>
#                                              create the next version's tag on
#                                              sha through the GitHub API and
#                                              print the version (or the one sha
#                                              already has; nothing before the
#                                              first release)
#   deploy/release.sh next <patch|minor> <version>
#                                              the version after it
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
latest)
  # Only plain X.Y.Z tags start the line: a -rc tag doesn't.
  if [ "${2:-}" = --all ]; then merged=""; else merged="--merged=${2:-HEAD}"; fi
  git tag ${merged} --list 'v*' |
    sed -n 's/^v\(\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\.\(0\|[1-9][0-9]*\)\)$/\1/p' |
    sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1
  ;;
next)
  kind="${2:-}"
  v="${3:-}"
  printf '%s\n' "$v" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
    die "next: '$v' is not X.Y.Z"
  major="${v%%.*}"; rest="${v#*.}"; minor="${rest%%.*}"; patch="${rest#*.}"
  case "$kind" in
  patch) echo "$major.$minor.$((patch + 1))" ;;
  minor) echo "$major.$((minor + 1)).0" ;;
  *) die "next: kind must be patch or minor" ;;
  esac
  ;;
reserve)
  # Every merge gets its own version, even when merges race: the tag is
  # created through the API, which refuses one that exists; the loser
  # fetches the tags and takes the next. Needs GH_TOKEN and
  # GITHUB_REPOSITORY (LOOM-129).
  sha="${2:-}"
  kind="${3:-patch}"
  existing="$(git tag --points-at "$sha" --list 'v*' |
    sed -n 's/^v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$/\1/p' | head -n 1)"
  if [ -n "$existing" ]; then echo "$existing"; exit 0; fi
  err="$(mktemp)"
  trap 'rm -f "$err"' EXIT
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    git fetch -q --tags --force origin 2>/dev/null || true
    latest="$("$0" latest --all)"
    if [ -z "$latest" ]; then
      [ "$kind" != minor ] || die "reserve: no release line yet; cut the first version by hand"
      exit 0
    fi
    v="$("$0" next "$kind" "$latest")"
    if gh api "repos/${GITHUB_REPOSITORY}/git/refs" -f ref="refs/tags/v$v" -f sha="$sha" >/dev/null 2>"$err"; then
      git tag "v$v" "$sha" 2>/dev/null || true
      echo "$v"
      exit 0
    fi
    grep -q 'Reference already exists' "$err" || die "reserve: creating tag v$v failed: $(cat "$err")"
    sleep 1
  done
  die "reserve: gave up after 10 tries"
  ;;
*)
  die "usage: $0 version <tag> | prerelease <version> | notes <version> [file] | latest [rev|--all] | next <patch|minor> <version> | reserve <sha> <patch|minor>"
  ;;
esac
