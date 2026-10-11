#!/usr/bin/env bash
# conformance-images.sh — list the images the conformance and e2e suites run.
#
# Prints one name:tag per line, read from the vendored conformance manifests
# and from the e2e suite's echoBasicImage constant, so conformance-setup.sh
# can preload them into kind instead of leaving each node to pull them while
# a suite waits.
set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manifests="${repo_root}/vendor/sigs.k8s.io/gateway-api/conformance"
e2e_setup="${repo_root}/test/e2e/setup_test.go"

[[ -d "${manifests}" ]] || die "${manifests} not found; run go mod vendor"
[[ -f "${e2e_setup}" ]] || die "${e2e_setup} not found"

conformance_images="$(grep --recursive --no-filename --include='*.yaml' \
  --extended-regexp '^[[:space:]]+image: ' "${manifests}" | awk '{print $2}' || true)"
[[ -n "${conformance_images}" ]] || die "no image found in ${manifests}"

e2e_image="$(sed -n 's/^const echoBasicImage = "\(.*\)"$/\1/p' "${e2e_setup}")"
[[ -n "${e2e_image}" ]] || die "no echoBasicImage constant in ${e2e_setup}"

printf '%s\n' "${conformance_images}" "${e2e_image}" | sort --unique
