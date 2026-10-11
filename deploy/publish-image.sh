#!/bin/sh
# Publishes a tested local image under its tags, keeping version and
# commit tags immutable (LOOM-194; docs/release/versioning.md):
#
#   deploy/publish-image.sh <image> <revision> <local-ref> <tag>...
#
# prints the digest every tag ends up naming. `main` is the only moving
# tag. Every other tag is immutable: one that already exists is accepted
# only when its org.opencontainers.image.revision label is <revision>
# (a re-run of the same commit), and then its digest is what the other
# tags get rather than the local image's (a rebuild isn't reproducible,
# so the registry's copy of this commit wins). An existing immutable
# tag built from another commit fails the run, and so do two existing
# tags of this commit that disagree. Tags that don't exist yet all get
# one digest: the local image is pushed under the first missing
# immutable tag, and every other tag is pointed at that digest in the
# registry without a second upload.
#
# Needs docker (logged in; buildx for the re-tagging) and deploy/ghcr.sh
# (GHCR names another reader: tests).
set -eu

ghcr="${GHCR:-$(dirname "$0")/ghcr.sh}"
die() { echo "publish-image: $*" >&2; exit 1; }
[ $# -ge 4 ] || die "usage: $0 <image> <revision> <local-ref> <tag>..."
image="$1"
revision="$2"
local_ref="$3"
shift 3

digest=""
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
    if [ -z "$digest" ]; then
      digest="$existing"
      echo "publish-image: ${image}:${tag} already exists for ${revision} as ${digest}; using it" >&2
    elif [ "$existing" != "$digest" ]; then
      die "${image}:${tag} is ${existing} but another tag of ${revision} is ${digest}: two images for one commit; sort the registry out by hand"
    fi
    ;;
  2) pending="$pending $tag" ;;
  *) die "reading ${image}:${tag} failed" ;;
  esac
done

if [ -z "$digest" ]; then
  [ -n "$pending" ] || die "no immutable tag given (main alone can't be published)"
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
