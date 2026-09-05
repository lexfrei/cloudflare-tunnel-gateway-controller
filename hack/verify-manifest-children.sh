#!/usr/bin/env bash
# verify-manifest-children.sh — assert a published manifest index was assembled
# out of the images this build job pushed.
#
# The merge job reads its manifest-list digest back from a tag, and ttl.sh has
# no authentication while the run id in that tag is public for as long as the
# run is. In the seconds between the manifest push and the read-back, anyone can
# repoint that tag. The digest read back would then be theirs, and every check
# downstream would accept it, because it really is a well-formed content address
# to a real image. It is just not this build.
#
# What `/tmp/digests` names is not what the published index lists. Provenance is
# on, so each per-arch build pushes an OCI index of its own -- a platform
# manifest plus an attestation manifest -- and `imagetools create` flattens
# index sources, so the published index carries those children and never the
# source digests. Each pushed digest is therefore resolved to its own index, and
# it is that index's children that have to appear.
#
# Usage: verify-manifest-children.sh <image> <published-index-json> <digests-dir>

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -eq 3 ]] || die "usage: $0 <image> <published-index-json> <digests-dir>"

image="$1"
published="$2"
digests_dir="$3"

[[ -f "${published}" ]] || die "manifest index ${published} does not exist"
[[ -d "${digests_dir}" ]] || die "digests directory ${digests_dir} does not exist"

checked=0
for digest_file in "${digests_dir}"/*; do
  [[ -f "${digest_file}" ]] || continue
  source_digest="sha256:${digest_file##*/}"

  raw="$(docker buildx imagetools inspect "${image}@${source_digest}" --raw)" \
    || die "cannot read ${source_digest}, which this job pushed"

  # With provenance off a build pushes a plain manifest instead of an index. It
  # has no children, and then the source digest is itself what must appear.
  expected="$(jq --raw-output '.manifests[]?.digest' <<< "${raw}")"
  [[ -n "${expected}" ]] || expected="${source_digest}"

  while read -r want; do
    jq --exit-status --arg want "${want}" \
      'any(.manifests[]?; .digest == $want)' "${published}" > /dev/null \
      || die "the published index does not list ${want}, pushed under ${source_digest}"
    checked=$((checked + 1))
  done <<< "${expected}"
done

# Without this an empty directory would satisfy the loop vacuously, and the
# check would wave through any index at all.
[[ "${checked}" -gt 0 ]] || die "no pushed digests to check in ${digests_dir}"

echo "published index lists all ${checked} manifests this job pushed"
