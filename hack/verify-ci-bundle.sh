#!/usr/bin/env bash
# verify-ci-bundle.sh — validate the artifacts a PR's CI run published before
# anything is deployed from them.
#
# The bundle is assembled by hack/conformance-setup.sh --use-ci-images from
# three artifacts of one "PR Checks and Build" run:
#   controller.ref / proxy.ref   image references, digest-pinned
#   head-sha.txt                 commit the run built
#   <chart>.tgz                  the packaged chart
#
# Everything downstream deploys what these files name, with the maintainer's
# Cloudflare credentials mounted into it. A reference that is not digest-pinned
# would put a mutable ttl.sh tag back on that path, so it has to stop the run
# rather than degrade quietly.
#
# Usage: verify-ci-bundle.sh <bundle-dir> <pr-number> <expected-head-sha>
# Prints `key=value` lines for the caller on success.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -eq 3 ]] || die "usage: $0 <bundle-dir> <pr-number> <expected-head-sha>"

bundle_dir="$1"
pr_number="$2"
expected_head_sha="$3"

[[ -d "${bundle_dir}" ]] || die "bundle directory ${bundle_dir} does not exist"
[[ "${pr_number}" =~ ^[0-9]+$ ]] || die "PR number must be numeric, got '${pr_number}'"

read_ref() {
  local name="$1" image="$2" file="${bundle_dir}/$1" value
  [[ -f "${file}" ]] || die "${name} missing from the CI bundle"
  value="$(tr -d '[:space:]' < "${file}")"
  # Both halves are pinned. The digest half must be a full content address, so
  # "repo:tag" and a short digest fail. The repository half must be the one
  # image this file is for: a valid digest on someone else's registry must not
  # be deployed with real credentials, and the two CI images must not be
  # swappable, which would run the controller binary in the proxy Deployment.
  [[ "${value}" =~ ^"${image}"@sha256:[0-9a-f]{64}$ ]] \
    || die "${name} is not a digest-pinned reference to ${image}: '${value}'"
  printf '%s' "${value}"
}

controller_ref="$(read_ref controller.ref ttl.sh/cf-tunnel-gateway-ctrl)"
proxy_ref="$(read_ref proxy.ref ttl.sh/cf-tunnel-gateway-proxy)"

head_sha_file="${bundle_dir}/head-sha.txt"
[[ -f "${head_sha_file}" ]] || die "head-sha.txt missing from the CI bundle"
head_sha="$(tr -d '[:space:]' < "${head_sha_file}")"
# Binds the artifacts to the commit, not just the run that claims to have
# built them: a bundle whose chart was packaged from another revision is the
# case this refuses.
[[ "${head_sha}" == "${expected_head_sha}" ]] \
  || die "CI bundle was built from ${head_sha:-nothing}, expected ${expected_head_sha}"

chart_version="0.0.0-pr.${pr_number}-1d"
chart_tgz="${bundle_dir}/cloudflare-tunnel-gateway-controller-${chart_version}.tgz"
[[ -f "${chart_tgz}" ]] || die "chart ${chart_tgz##*/} missing from the CI bundle"

echo "head_sha=${head_sha}"
echo "controller_ref=${controller_ref}"
echo "proxy_ref=${proxy_ref}"
echo "chart_tgz=${chart_tgz}"
