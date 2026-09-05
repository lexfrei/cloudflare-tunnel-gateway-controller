#!/usr/bin/env bash
# verify-manifest-children.sh — assert a published manifest index still lists
# the per-arch images the build job pushed.
#
# The merge job reads its manifest-list digest back from a tag, and ttl.sh has
# no authentication while the run id in that tag is public for as long as the
# run is. In the seconds between the push and the read-back, anyone can repoint
# that tag; the digest read back would then be theirs, and every check
# downstream would pass it, because it really is a well-formed content address
# to a real image. It is just not this build.
#
# The per-arch digests are the part an attacker cannot reproduce, so the index
# has to carry all of them. Containment rather than set equality: buildx
# attaches provenance and SBOM manifests of its own.
#
# Usage: verify-manifest-children.sh <index-json> <digests-dir>
# <digests-dir> holds one empty file per pushed image, named by its bare digest
# -- the layout `docker/build-push-action` and the merge job already use.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -eq 2 ]] || die "usage: $0 <index-json> <digests-dir>"

index_json="$1"
digests_dir="$2"

[[ -f "${index_json}" ]] || die "manifest index ${index_json} does not exist"
[[ -d "${digests_dir}" ]] || die "digests directory ${digests_dir} does not exist"

checked=0
for digest_file in "${digests_dir}"/*; do
  [[ -f "${digest_file}" ]] || continue
  want="sha256:${digest_file##*/}"
  jq --exit-status --arg want "${want}" \
    'any(.manifests[]?; .digest == $want)' "${index_json}" > /dev/null \
    || die "the published index does not list ${want}, which this job pushed"
  checked=$((checked + 1))
done

# Without this an empty directory would satisfy the loop vacuously, and the
# check would wave through any index at all.
[[ "${checked}" -gt 0 ]] || die "no pushed digests to check in ${digests_dir}"

echo "index lists all ${checked} manifests this job pushed"
