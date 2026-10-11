#!/bin/sh
# Publishes a tested local image under its tags, keeping version and
# commit tags immutable (LOOM-194; docs/release/versioning.md):
#
#   deploy/publish-image.sh <image> <revision> <version-label> <local-ref> <tag>...
#
# prints the digest every tag ends up naming. <version-label> is what
# this run labels its image (org.opencontainers.image.version: the
# release, or sha-<short> without one). `main` is the only moving tag.
# Every other tag is immutable:
#
#   - one that already exists must have been built from <revision> (its
#     org.opencontainers.image.revision label), or the run fails;
#   - built from this commit under this run's version label, it is a
#     re-run's twin: its digest is adopted and the other tags get it,
#     rather than the local image (a rebuild isn't reproducible, so the
#     registry's copy wins); two such tags that disagree fail the run;
#   - built from this commit under another version label (a hand-pushed
#     MINOR on a commit the merge already released as a patch: the
#     commit tags carry the patch's label), it is left alone, neither
#     adopted nor re-pushed, since it names that commit's first build;
#     the image built as this version is pushed under the version tag.
#
# Tags that don't exist yet all get one digest: the local image is
# pushed under the first missing immutable tag, and every other tag is
# pointed at that digest in the registry without a second upload. When
# nothing is missing and nothing was adopted (a plain rebuild of a
# released commit), nothing is pushed and the commit's existing digest
# is reported.
#
# Needs docker (logged in; buildx for the re-tagging) and deploy/ghcr.sh
# (GHCR names another reader: tests).
set -eu

ghcr="${GHCR:-$(dirname "$0")/ghcr.sh}"
die() { echo "publish-image: $*" >&2; exit 1; }
[ $# -ge 5 ] || die "usage: $0 <image> <revision> <version-label> <local-ref> <tag>..."
image="$1"
revision="$2"
version_label="$3"
local_ref="$4"
shift 4

digest=""
existing_other=""
pending=""
moving=""
for tag in "$@"; do
  case "$tag" in
  main) moving="$moving $tag"; continue ;;
  esac
  set +e
  existing="$("$ghcr" digest "$image" "$tag")"
  code=$?
  set -e
  case "$code" in
  0)
    rev="$("$ghcr" label "$image" "$tag" org.opencontainers.image.revision)"
    [ "$rev" = "$revision" ] ||
      die "${image}:${tag} exists and was built from commit '${rev:-unknown}', not ${revision}: a version or commit tag is never re-pushed"
    ver="$("$ghcr" label "$image" "$tag" org.opencontainers.image.version)"
    if [ "$ver" != "$version_label" ]; then
      echo "publish-image: ${image}:${tag} exists for ${revision} as version '${ver}', not ${version_label}; leaving it" >&2
      existing_other="${existing_other:-$existing}"
      continue
    fi
    if [ -z "$digest" ]; then
      digest="$existing"
      echo "publish-image: ${image}:${tag} already exists for ${revision} (${version_label}) as ${digest}; using it" >&2
    elif [ "$existing" != "$digest" ]; then
      die "${image}:${tag} is ${existing} but another tag of ${revision} is ${digest}: two images for one commit; sort the registry out by hand"
    fi
    ;;
  2) pending="$pending $tag" ;;
  *) die "reading ${image}:${tag} failed" ;;
  esac
done

if [ -z "$digest" ] && [ -z "$pending" ]; then
  [ -n "$existing_other" ] || die "no immutable tag given (main alone can't be published)"
  echo "publish-image: every tag of ${revision} already exists; nothing to publish" >&2
  digest="$existing_other"
fi
if [ -z "$digest" ]; then
  first="${pending# }"
  first="${first%% *}"
  pending="${pending# }"
  pending="${pending#"$first"}"
  docker tag "$local_ref" "${image}:${first}"
  digest="$(docker push "${image}:${first}" | tee /dev/stderr |
    sed -n 's/.*digest: \(sha256:[0-9a-f]*\).*/\1/p' | tail -n 1)"
  [ -n "$digest" ] || die "the push of ${image}:${first} reported no digest"
fi
for tag in $pending $moving; do
  docker buildx imagetools create -t "${image}:${tag}" "${image}@${digest}" >&2
done
printf '%s\n' "$digest"
