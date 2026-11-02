#!/bin/sh
# The commit a published image was built from (its
# org.opencontainers.image.revision label), asked anonymously from GHCR:
#
#   deploy/reflux/image-revision.sh ghcr.io/belebob/reflux-node:main
#
# Prints the commit, or nothing when the image or its label is not there.
# Needs curl and jq.
set -eu

ref=${1#ghcr.io/}
repo=${ref%:*}
tag=${ref##*:}
api=https://ghcr.io/v2/$repo

token=$(curl -fsS "https://ghcr.io/token?scope=repository:$repo:pull" | jq -r .token) || exit 0
get() {
	curl -fsSL -H "Authorization: Bearer $token" -H "Accept: $1" "$api/$2"
}

accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
manifest=$(get "$accept" "manifests/$tag") || exit 0
# An index (buildx adds attestations): the linux/amd64 image in it.
digest=$(printf '%s' "$manifest" | jq -r '[.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64")][0].digest // empty')
if [ -n "$digest" ]; then
	manifest=$(get "$accept" "manifests/$digest") || exit 0
fi
config=$(printf '%s' "$manifest" | jq -r '.config.digest // empty')
[ -n "$config" ] || exit 0
get 'application/json' "blobs/$config" | jq -r '.config.Labels["org.opencontainers.image.revision"] // empty' || true
