#!/bin/sh
# Reads a public image on GHCR without docker (LOOM-194): the manifest
# digest a tag names, and the image config's labels. The release run
# uses it to reuse the current agent image when deploy/agent didn't
# change (.github/workflows/agent.yml) and to keep version tags
# immutable (deploy/publish-image.sh).
#
#   deploy/ghcr.sh digest <image> <tag>          the digest the tag names
#   deploy/ghcr.sh labels <image> <tag>          the config labels, as JSON
#   deploy/ghcr.sh label  <image> <tag> <name>   one label ("" when unset)
#   deploy/ghcr.sh tag    <image> <digest> <tag> point <tag> at the manifest
#                                                <digest> names: its bytes are
#                                                copied as they are, so the tag
#                                                names that same digest (what
#                                                an attestation covers); needs
#                                                GHCR_USER and GHCR_PASSWORD
#
# <image> is ghcr.io/<owner>/<name>. For an image index (multi-platform,
# or an image with attestation manifests beside it) the digest is the
# index's, which is what the tag names and what a pull pins, and the
# labels are the linux/amd64 image's. Exit 2 when the tag doesn't exist,
# 1 on any other failure. Reads are anonymous, so public packages only
# (every loomux image is). GHCR_REGISTRY overrides https://ghcr.io
# (tests).
set -eu

registry="${GHCR_REGISTRY:-https://ghcr.io}"
accept='application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json'

die() { echo "ghcr: $*" >&2; exit 1; }
usage() { die "usage: $0 digest|labels <image> <tag> | label <image> <tag> <name> | tag <image> <digest> <tag>"; }

cmd="${1:-}"
image="${2:-}"
tag="${3:-}"
name="${4:-}"
scope=pull
case "$cmd" in
digest | labels) [ $# -eq 3 ] || usage ;;
label) [ $# -eq 4 ] || usage ;;
tag)
  [ $# -eq 4 ] || usage
  [ -n "${GHCR_USER:-}" ] && [ -n "${GHCR_PASSWORD:-}" ] || die "tag needs GHCR_USER and GHCR_PASSWORD"
  scope=pull,push
  ;;
*) usage ;;
esac
repo="${image#ghcr.io/}"
case "$repo" in
*/*/* | */ | /* | *:* | "") die "image '$image' isn't ghcr.io/<owner>/<name>" ;;
*/*) ;;
*) die "image '$image' isn't ghcr.io/<owner>/<name>" ;;
esac

body="$(mktemp)"
hdr="$(mktemp)"
trap 'rm -f "$body" "$hdr"' EXIT

if [ "$scope" = pull ]; then
  token="$(curl -fsS "${registry}/token?scope=repository:${repo}:${scope}" | jq -r '.token // empty')"
else
  token="$(curl -fsS -u "${GHCR_USER}:${GHCR_PASSWORD}" "${registry}/token?scope=repository:${repo}:${scope}" | jq -r '.token // empty')"
fi
[ -n "$token" ] || die "no ${scope} token for ${repo}"

# manifest <ref>: the manifest into $body, its digest on stdout. Exit 2
# for a tag that doesn't exist.
manifest() {
  code="$(curl -sS -o "$body" -D "$hdr" -w '%{http_code}' \
    -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
    "${registry}/v2/${repo}/manifests/$1")"
  case "$code" in
  200) ;;
  404) exit 2 ;;
  *) die "GET ${repo}/manifests/$1: HTTP ${code}" ;;
  esac
  d="$(tr -d '\r' < "$hdr" | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p' | tail -n 1)"
  if [ -z "$d" ]; then d="sha256:$(sha256sum "$body" | cut -d' ' -f1)"; fi
  printf '%s\n' "$d"
}

if [ "$cmd" = tag ]; then
  # tag <image> <digest> <tag>: $tag holds the digest, $name the tag.
  want="$tag"
  got="$(manifest "$want")" || die "${image}@${want}: no such manifest"
  [ "$got" = "$want" ] || die "${image}@${want} answered as ${got}"
  type="$(tr -d '\r' < "$hdr" | sed -n 's/^[Cc]ontent-[Tt]ype: *//p' | tail -n 1)"
  [ -n "$type" ] || die "${image}@${want}: no content type"
  code="$(curl -sS -o /dev/null -D "$hdr" -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer ${token}" -H "Content-Type: ${type}" \
    --data-binary "@${body}" "${registry}/v2/${repo}/manifests/${name}")"
  case "$code" in 200 | 201 | 202) ;; *) die "PUT ${repo}/manifests/${name}: HTTP ${code}" ;; esac
  d="$(tr -d '\r' < "$hdr" | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p' | tail -n 1)"
  if [ -n "$d" ] && [ "$d" != "$want" ]; then
    die "${image}:${name} came out as ${d}, not ${want}"
  fi
  printf '%s\n' "$want"
  exit 0
fi

digest="$(manifest "$tag")"
if [ "$cmd" = digest ]; then
  printf '%s\n' "$digest"
  exit 0
fi

# An index: read the linux/amd64 image's manifest instead.
if jq -e '.manifests' "$body" >/dev/null 2>&1; then
  amd="$(jq -r '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")][0].digest // empty' "$body")"
  [ -n "$amd" ] || die "${image}:${tag} has no linux/amd64 image"
  manifest "$amd" >/dev/null
fi
config="$(jq -r '.config.digest // empty' "$body")"
[ -n "$config" ] || die "${image}:${tag}: no config in the manifest"
labels="$(curl -fsSL -H "Authorization: Bearer ${token}" "${registry}/v2/${repo}/blobs/${config}" | jq -c '.config.Labels // {}')"
case "$cmd" in
labels) printf '%s\n' "$labels" ;;
label) printf '%s' "$labels" | jq -r --arg n "$name" '.[$n] // empty' ;;
esac
