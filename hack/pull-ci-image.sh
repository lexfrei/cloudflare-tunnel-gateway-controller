#!/usr/bin/env bash
# pull-ci-image.sh — pull one CI image for this host's platform and tag it.
#
# `kind load docker-image` runs `docker save | ctr images import
# --all-platforms`. If the local tag carries a multi-arch index, ctr demands
# blobs for every platform in it, including the ones this host never fetched,
# and the import dies on the first foreign manifest. So resolve the host's own
# manifest out of the index the caller already verified and pull that: what
# ends up under the local tag is a single platform.
#
# The index digest stays the root of trust -- the platform digest is read out
# of that index, not resolved from a tag.
#
# Usage: pull-ci-image.sh <repo@sha256:index-digest> <local-tag> <goarch>

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -eq 3 ]] || die "usage: $0 <image-ref> <local-tag> <goarch>"

ref="$1"
local_tag="$2"
arch="$3"

index="$(docker buildx imagetools inspect "${ref}" --raw)" \
  || die "cannot read the image index for ${ref}"

digest="$(jq --raw-output --arg arch "${arch}" '
    .manifests[]
    | select(.platform.os == "linux" and .platform.architecture == $arch)
    | .digest' <<< "${index}")"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || die "${ref} carries no linux/${arch} manifest"

docker pull "${ref%@*}@${digest}"
docker tag "${ref%@*}@${digest}" "${local_tag}"
