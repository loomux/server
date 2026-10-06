#!/bin/sh
# Changelog fragments: each pull request adds its CHANGELOG entry as a
# file in changes/ instead of editing CHANGELOG.md, so two open PRs never
# conflict over the same [Unreleased] lines. A milestone (a MINOR, an rc,
# 1.0.0) folds them into CHANGELOG.md. Format: changes/README.md.
#
#   changelog.sh check                 fragments are well-formed (CI)
#   changelog.sh assemble              print them as Keep a Changelog sections
#   changelog.sh release VERSION DATE  write them into CHANGELOG.md as
#                                      "## [VERSION] - DATE" and delete them
set -eu

dir="${CHANGES_DIR:-changes}"
changelog="${CHANGELOG:-CHANGELOG.md}"
# Keep a Changelog's sections, in its order.
sections="Added Changed Deprecated Removed Fixed Security"

die() { echo "changelog: $*" >&2; exit 1; }

fragments() {
  for f in "$dir"/*.md; do
    [ -e "$f" ] || continue
    [ "$(basename "$f")" = README.md ] && continue
    echo "$f"
  done
}

check() {
  status=0
  for f in $(fragments); do
    if ! awk -v sections=" $sections " -v file="$f" '
      /^### / {
        name = substr($0, 5)
        if (index(sections, " " name " ") == 0) { print file ": unknown section \"" name "\" (use one of" sections ")"; bad = 1 }
        in_section = 1; next
      }
      /^[[:space:]]*$/ { next }
      !in_section { print file ": text before the first \"### Section\" heading"; bad = 1; exit }
      /^- / { items++ }
      END {
        if (!bad && items == 0) { print file ": no \"- \" entries"; bad = 1 }
        exit bad
      }' "$f" >&2; then
      status=1
    fi
  done
  return "$status"
}

assemble() {
  files=$(fragments)
  [ -n "$files" ] || return 0
  first=1
  # Entries are list items, blank lines between them dropped.
  for s in $sections; do
    # shellcheck disable=SC2086 # the file list is word-split on purpose
    body=$(awk -v want="$s" '
      FNR == 1 { take = 0 }
      /^### / { take = (substr($0, 5) == want); next }
      take && NF { print }' $files)
    [ -n "$body" ] || continue
    [ "$first" = 1 ] || echo
    first=0
    printf '### %s\n\n%s\n' "$s" "$body"
  done
}

release() {
  version="$1"
  date="$2"
  echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "version must be SemVer without a v, got $version"
  echo "$date" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' || die "date must be YYYY-MM-DD, got $date"
  check || die "fix the fragments first"
  grep -q "^## \[$version\]" "$changelog" && die "$changelog already has a [$version] section"
  body=$(assemble)
  [ -n "$body" ] || die "no fragments in $dir/ to release"
  repo_url=$(sed -n 's/^\[Unreleased\]: \(.*\)\/compare\/.*$/\1/p' "$changelog")
  [ -n "$repo_url" ] || die "$changelog has no [Unreleased] compare link"
  tmp="$changelog.tmp"
  BODY="$body" awk -v version="$version" -v date="$date" -v url="$repo_url" '
    /^## \[Unreleased\]/ && !done {
      print; getline
      # keep the Unreleased section'"'"'s own note, then the new section
      while ($0 !~ /^## \[/ && $0 !~ /^\[Unreleased\]:/) { print; if ((getline) <= 0) break }
      print "## [" version "] - " date "\n\n" ENVIRON["BODY"] "\n"
      done = 1
    }
    /^\[Unreleased\]: / { print "[Unreleased]: " url "/compare/v" version "...HEAD"; print "[" version "]: " url "/releases/tag/v" version; next }
    { print }' "$changelog" >"$tmp"
  mv "$tmp" "$changelog"
  for f in $(fragments); do rm -f "$f"; done
  echo "changelog: wrote [$version] into $changelog and removed its fragments; commit both"
}

case "${1:-}" in
check) check ;;
assemble) assemble ;;
release) [ $# -eq 3 ] || die "usage: $0 release VERSION DATE"; release "$2" "$3" ;;
*) die "usage: $0 check | assemble | release VERSION DATE" ;;
esac
