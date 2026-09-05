#!/usr/bin/env bash
# conformance-setup_test.sh — tests for conformance-setup.sh and the CI-bundle
# verifier it calls.
#
# Two properties are pinned here. The colima prerequisite is a macOS
# requirement rather than a "not in CI" requirement, so a Linux host with a
# native docker daemon gets past the tool check. And what --use-ci-images
# deploys is addressed by digest and bound to the reviewed commit: a mutable
# tag or a bundle from another revision must stop the run. Plain bash and temp
# dirs, no test framework.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
setup="${script_dir}/conformance-setup.sh"
verifier="${script_dir}/verify-ci-bundle.sh"
puller="${script_dir}/pull-ci-image.sh"
repo_root="$(cd "${script_dir}/.." && pwd)"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail=0

pass() { echo "ok   - $1"; }
flunk() { echo "FAIL - $1"; fail=1; }

# --- conformance-setup.sh prerequisite block -------------------------------
#
# The script is copied into a throwaway REPO_ROOT so the checkout's own .env
# can never satisfy the credential check, and run with a PATH that holds
# nothing but no-op stubs plus the system coreutils. colima is deliberately
# absent from that PATH on both hosts, and `uname` is stubbed, so the same
# assertions hold whether the suite runs on Linux or macOS.
sandbox="${tmp}/sandbox"
stubs="${tmp}/stubs"
mkdir -p "${sandbox}/hack" "${stubs}"
cp "${setup}" "${sandbox}/hack/"

for stub in docker kind helm kubectl go; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stubs}/${stub}"
  chmod +x "${stubs}/${stub}"
done

# run_prereq <uname-s-output> -> stdout+stderr of the script, exit code ignored
run_prereq() {
  local kernel="$1"
  cat > "${stubs}/uname" <<STUB
#!/usr/bin/env bash
if [[ "\$1" == "-s" ]]; then echo ${kernel}; fi
exit 0
STUB
  chmod +x "${stubs}/uname"
  # GITHUB_ACTIONS and the CF_* variables are cleared explicitly: the suite
  # itself runs in Actions, where inheriting them would skip the very branch
  # under test and satisfy the credential check from the ambient environment.
  PATH="${stubs}:/usr/bin:/bin" \
  GITHUB_ACTIONS='' CF_API_TOKEN='' CF_ACCOUNT_ID='' CF_TUNNEL_ID='' \
  CF_TUNNEL_TOKEN='' CF_TUNNEL_HOSTNAME='' \
    bash "${sandbox}/hack/conformance-setup.sh" 2>&1 || true
}

linux_out="$(run_prereq Linux)"
if grep --quiet "colima is not installed" <<< "${linux_out}"; then
  flunk "Linux host must not require colima"
else
  pass "Linux host does not require colima"
fi
# Getting as far as the credential check is what proves the tool check passed;
# without it "no colima error" would also be true of a script that died earlier.
if grep --quiet ".env file not found" <<< "${linux_out}"; then
  pass "Linux host reaches the credential check"
else
  flunk "Linux host reaches the credential check (got: ${linux_out##*$'\n'})"
fi

darwin_out="$(run_prereq Darwin)"
if grep --quiet "colima is not installed" <<< "${darwin_out}"; then
  pass "macOS host still requires colima"
else
  flunk "macOS host still requires colima"
fi

# --- verify-ci-bundle.sh ---------------------------------------------------

ctrl_ref="ttl.sh/cf-tunnel-gateway-ctrl@sha256:$(printf 'a%.0s' {1..64})"
proxy_ref="ttl.sh/cf-tunnel-gateway-proxy@sha256:$(printf 'b%.0s' {1..64})"
head_sha="$(printf 'c%.0s' {1..40})"

# make_bundle <dir> <pr> [head-sha]
make_bundle() {
  local dir="$1" pr="$2" sha="${3:-${head_sha}}"
  mkdir -p "${dir}"
  printf '%s\n' "${ctrl_ref}" > "${dir}/controller.ref"
  printf '%s\n' "${proxy_ref}" > "${dir}/proxy.ref"
  printf '%s\n' "${sha}" > "${dir}/head-sha.txt"
  : > "${dir}/cloudflare-tunnel-gateway-controller-0.0.0-pr.${pr}-1d.tgz"
}

# check_bundle <expected-exit> <label> <dir>
check_bundle() {
  local expected="$1" label="$2" dir="$3" actual=0
  bash "${verifier}" "${dir}" 720 "${head_sha}" >/dev/null 2>&1 || actual=$?
  if [[ "${actual}" -eq "${expected}" ]]; then
    pass "${label} (exit ${actual})"
  else
    flunk "${label}: expected exit ${expected}, got ${actual}"
  fi
}

make_bundle "${tmp}/good" 720
check_bundle 0 "complete bundle accepted" "${tmp}/good"

out="$(bash "${verifier}" "${tmp}/good" 720 "${head_sha}" || true)"
if grep --quiet "^controller_ref=${ctrl_ref}$" <<< "${out}" \
  && grep --quiet "^proxy_ref=${proxy_ref}$" <<< "${out}" \
  && grep --quiet "^head_sha=${head_sha}$" <<< "${out}"; then
  pass "verifier echoes the references it validated"
else
  flunk "verifier echoes the references it validated"
fi

# The exposure the whole path exists to close: a mutable tag standing in for a
# content address.
make_bundle "${tmp}/tag" 720
printf 'ttl.sh/cf-tunnel-gateway-ctrl:pr-720-1d\n' > "${tmp}/tag/controller.ref"
check_bundle 1 "a tag in place of a digest is rejected" "${tmp}/tag"

# Each file is bound to its own image: swapping the two would run the
# controller binary in the proxy Deployment.
make_bundle "${tmp}/swap" 720
printf '%s\n' "${proxy_ref}" > "${tmp}/swap/controller.ref"
check_bundle 1 "the two CI image references are not swappable" "${tmp}/swap"

# A digest binds content, not provenance: a well-formed reference to an image
# nobody here built must not pass.
make_bundle "${tmp}/foreign" 720
printf 'ghcr.io/someone-else/cf-tunnel-gateway-ctrl@sha256:%s\n' "$(printf 'a%.0s' {1..64})" \
  > "${tmp}/foreign/controller.ref"
check_bundle 1 "a digest on a foreign registry is rejected" "${tmp}/foreign"

make_bundle "${tmp}/short" 720
printf 'ttl.sh/cf-tunnel-gateway-proxy@sha256:%s\n' "$(printf 'a%.0s' {1..63})" \
  > "${tmp}/short/proxy.ref"
check_bundle 1 "a truncated digest is rejected" "${tmp}/short"

make_bundle "${tmp}/noref" 720
rm -f "${tmp}/noref/proxy.ref"
check_bundle 1 "a missing reference file is rejected" "${tmp}/noref"

# Artifacts built from a revision other than the one being verified.
make_bundle "${tmp}/othersha" 720 "$(printf 'd%.0s' {1..40})"
check_bundle 1 "a bundle built from another commit is rejected" "${tmp}/othersha"

# A bundle downloaded from another PR's run carries that PR's chart version.
make_bundle "${tmp}/otherpr" 721
check_bundle 1 "a bundle from another PR is rejected" "${tmp}/otherpr"

check_bundle 1 "a missing bundle directory is rejected" "${tmp}/absent"

# --- wiring ----------------------------------------------------------------
# The verifier only protects anything if the setup script actually runs it.
if grep --quiet "verify-ci-bundle.sh" "${setup}"; then
  pass "conformance-setup.sh invokes the bundle verifier"
else
  flunk "conformance-setup.sh invokes the bundle verifier"
fi

# ttl.sh drops the images long before the run's artifacts expire, so the pull
# is the likely failure and it must happen before the script deletes the
# operator's existing clusters.
pull_line="$(grep --line-number --fixed-strings 'pull-ci-image.sh' "${setup}" | head -1 | cut -d: -f1)"
delete_line="$(grep --line-number 'kind delete cluster' "${setup}" | head -1 | cut -d: -f1)"
if [[ -n "${pull_line}" && -n "${delete_line}" && "${pull_line}" -lt "${delete_line}" ]]; then
  pass "CI images are pulled before any cluster is deleted"
else
  flunk "CI images are pulled before any cluster is deleted (pull ${pull_line:-none}, delete ${delete_line:-none})"
fi

# --- pull-ci-image.sh --------------------------------------------------------
#
# `kind load docker-image` saves the local tag and imports it with
# --all-platforms, so whatever carries that tag must be one platform. These
# cases pin that the puller resolves the host's manifest out of the index
# rather than tagging the index itself.

pull_stub_dir="${tmp}/pullstubs"
mkdir -p "${pull_stub_dir}"
cat > "${pull_stub_dir}/docker" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "buildx" ]]; then
  [[ -f "${FIXTURE_INDEX}" ]] || exit 1
  cat "${FIXTURE_INDEX}"
  exit 0
fi
echo "$*" >> "${DOCKER_LOG}"
STUB
chmod +x "${pull_stub_dir}/docker"

index_with_arches() {
  local out="$1"; shift
  {
    printf '{"manifests":['
    local sep=""
    for entry in "$@"; do
      printf '%s{"digest":"sha256:%s","platform":{"os":"linux","architecture":"%s"}}' \
        "${sep}" "$(printf '%s' "${entry%%:*}" | head -c 64)" "${entry##*:}"
      sep=","
    done
    printf ']}'
  } > "${out}"
}

# run_pull <fixture> <arch> -> exit code; DOCKER_LOG holds the recorded calls
run_pull() {
  local fixture="$1" arch="$2"
  : > "${tmp}/docker.log"
  PATH="${pull_stub_dir}:/usr/bin:/bin" FIXTURE_INDEX="${fixture}" DOCKER_LOG="${tmp}/docker.log" \
    bash "${puller}" "ttl.sh/cf-tunnel-gateway-ctrl@sha256:$(printf 'e%.0s' {1..64})" controller:dev "${arch}" \
    >/dev/null 2>&1
}

amd64_digest="$(printf 'a%.0s' {1..64})"
arm64_digest="$(printf 'b%.0s' {1..64})"
index_with_arches "${tmp}/index.json" "${amd64_digest}:amd64" "${arm64_digest}:arm64"

if run_pull "${tmp}/index.json" amd64; then
  pass "the host platform's manifest is pulled from a multi-arch index"
else
  flunk "the host platform's manifest is pulled from a multi-arch index"
fi
# Tagging the index instead of the platform manifest is the bug: ctr then
# demands blobs for an architecture this host never fetched.
if grep --quiet "pull ttl.sh/cf-tunnel-gateway-ctrl@sha256:${amd64_digest}$" "${tmp}/docker.log" \
  && grep --quiet "tag ttl.sh/cf-tunnel-gateway-ctrl@sha256:${amd64_digest} controller:dev$" "${tmp}/docker.log"; then
  pass "the local tag points at one platform, not at the index"
else
  flunk "the local tag points at one platform, not at the index (log: $(tr '\n' '; ' < "${tmp}/docker.log"))"
fi

index_with_arches "${tmp}/index-arm.json" "${arm64_digest}:arm64"
if run_pull "${tmp}/index-arm.json" amd64; then
  flunk "an index without the host platform is rejected"
else
  pass "an index without the host platform is rejected"
fi

if run_pull "${tmp}/absent.json" amd64; then
  flunk "an unreadable index is rejected"
else
  pass "an unreadable index is rejected"
fi

# --- verify-manifest-children.sh -------------------------------------------
#
# The merge job reads its manifest-list digest back from a run-scoped tag on
# ttl.sh, which has no authentication and whose run id is public for as long as
# the run is visible. These cases pin that the index that tag resolves to is
# checked against the per-arch images the job actually pushed, so a substituted
# index cannot be published as this run's reference.

children="${script_dir}/verify-manifest-children.sh"

amd64_pushed="$(printf '1%.0s' {1..64})"
arm64_pushed="$(printf '2%.0s' {1..64})"

# The merge job names each pushed image by an empty file in /tmp/digests.
mkdir -p "${tmp}/pushed"
touch "${tmp}/pushed/${amd64_pushed}" "${tmp}/pushed/${arm64_pushed}"

# check_children <expected-exit> <label> <index> <digests-dir>
check_children() {
  local expected="$1" label="$2" index="$3" dir="$4" actual=0
  bash "${children}" "${index}" "${dir}" >/dev/null 2>&1 || actual=$?
  if [[ "${actual}" -eq "${expected}" ]]; then
    pass "${label} (exit ${actual})"
  else
    flunk "${label}: expected exit ${expected}, got ${actual}"
  fi
}

index_with_arches "${tmp}/children-ok.json" "${amd64_pushed}:amd64" "${arm64_pushed}:arm64"
check_children 0 "an index assembled from this job's images is accepted" \
  "${tmp}/children-ok.json" "${tmp}/pushed"

# The exposure: between the push and the read-back anyone can repoint the tag,
# and the digest read back would then be theirs -- well-formed, digest-pinned,
# and not this build.
index_with_arches "${tmp}/children-sub.json" \
  "$(printf '9%.0s' {1..64}):amd64" "$(printf '8%.0s' {1..64}):arm64"
check_children 1 "a substituted index is rejected" \
  "${tmp}/children-sub.json" "${tmp}/pushed"

index_with_arches "${tmp}/children-partial.json" \
  "${amd64_pushed}:amd64" "$(printf '8%.0s' {1..64}):arm64"
check_children 1 "an index missing one pushed manifest is rejected" \
  "${tmp}/children-partial.json" "${tmp}/pushed"

# buildx attaches provenance and SBOM manifests of its own, so the check is
# containment rather than set equality.
printf '{"manifests":[{"digest":"sha256:%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:%s","platform":{"os":"unknown","architecture":"unknown"}}]}' \
  "${amd64_pushed}" "${arm64_pushed}" "$(printf '7%.0s' {1..64})" \
  > "${tmp}/children-attest.json"
check_children 0 "attestation manifests do not trip the check" \
  "${tmp}/children-attest.json" "${tmp}/pushed"

# An empty directory would make the containment loop vacuously true, and every
# index would pass.
mkdir -p "${tmp}/nodigests"
check_children 1 "an empty digest set is rejected" \
  "${tmp}/children-ok.json" "${tmp}/nodigests"

check_children 1 "an unreadable index is rejected" \
  "${tmp}/absent-index.json" "${tmp}/pushed"

# The check only protects anything if the workflow runs it. Matching the
# invocation rather than the name: the job's checkout step names the script in
# a comment, which would satisfy a bare filename grep on its own.
if grep --quiet --extended-regexp \
  '^[[:space:]]*hack/verify-manifest-children\.sh ' \
  "${repo_root}/.github/workflows/pr.yaml"; then
  pass "pr.yaml verifies the manifest list it publishes"
else
  flunk "pr.yaml verifies the manifest list it publishes"
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "conformance-setup tests FAILED"
  exit 1
fi
echo "conformance-setup tests passed"
