#!/bin/sh
# Prints the current multi-arch digest of every pinned base image, ready to paste into
# Dockerfile, runner/docker-compose.yml and .forgejo/workflows/ci.yml (see docs/MAINTENANCE.md).
#
#   scripts/update-pins.sh            # print "tag@digest" lines
#   scripts/update-pins.sh --check    # exit 1 if any pin in the repo differs from the registry
#
# Needs one of: docker (buildx imagetools), crane, skopeo+jq. It only reads registry metadata;
# nothing is pulled or run.
set -eu
cd "$(dirname "$0")/.."

# Keep this list in sync with the pins in the files named above.
IMAGES="golang:1.27
gcr.io/distroless/static-debian13:nonroot
quay.io/buildah/stable:latest
code.forgejo.org/forgejo/runner:6
node:20-bookworm"

digest() {
  ref="$1"
  if command -v crane >/dev/null 2>&1; then
    crane digest "$ref"
  elif command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then
    docker buildx imagetools inspect "$ref" --format '{{json .Manifest.Digest}}' | tr -d '"'
  elif command -v skopeo >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    skopeo inspect --raw "docker://$ref" | sha256sum | sed 's/^/sha256:/; s/ .*//'
  else
    echo "update-pins: need crane, docker buildx, or skopeo+jq" >&2
    exit 2
  fi
}

check=0
[ "${1:-}" = "--check" ] && check=1
stale=0
for ref in $IMAGES; do
  d=$(digest "$ref")
  [ -n "$d" ] || { echo "update-pins: no digest for $ref" >&2; exit 1; }
  if [ "$check" = 1 ]; then
    if grep -rqF "$ref@$d" Dockerfile runner/docker-compose.yml .forgejo/workflows/ci.yml; then
      echo "ok     $ref@$d"
    else
      echo "STALE  $ref -> $d"
      stale=1
    fi
  else
    echo "$ref@$d"
  fi
done
exit "$stale"
