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

# run_prereq <uname-s-output> [script-arg...] -> stdout+stderr of the script,
# exit code ignored. Stubs in ${extra_stubs}, when set, shadow the no-op ones;
# ${system_path} replaces the system directories on PATH.
extra_stubs=""
system_path="/usr/bin:/bin"
run_prereq() {
  local kernel="$1"
  shift
  cat > "${stubs}/uname" <<STUB
#!/usr/bin/env bash
if [[ "\$1" == "-s" ]]; then echo ${kernel}; fi
exit 0
STUB
  chmod +x "${stubs}/uname"
  # GITHUB_ACTIONS and the CF_* variables are cleared explicitly: the suite
  # itself runs in Actions, where inheriting them would skip the very branch
  # under test and satisfy the credential check from the ambient environment.
  PATH="${extra_stubs:+${extra_stubs}:}${stubs}:${system_path}" \
  GITHUB_ACTIONS='' CF_API_TOKEN='' CF_ACCOUNT_ID='' CF_TUNNEL_ID='' \
  CF_TUNNEL_TOKEN='' CF_TUNNEL_HOSTNAME='' \
    bash "${sandbox}/hack/conformance-setup.sh" "$@" 2>&1 || true
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

# --use-ci-images reads image indexes with `docker buildx`, a plugin the tool
# check cannot see by name. Missing, it surfaced much later as an unreadable
# index plus advice to re-run CI.
for stub in gh jq; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stubs}/${stub}"
  chmod +x "${stubs}/${stub}"
done
mkdir -p "${tmp}/nobuildx"
cat > "${tmp}/nobuildx/docker" <<'STUB'
#!/usr/bin/env bash
[[ "$1" == "buildx" ]] && exit 1
exit 0
STUB
chmod +x "${tmp}/nobuildx/docker"

nobuildx_out="$(extra_stubs="${tmp}/nobuildx" run_prereq Linux --use-ci-images 733)"
if grep --quiet "docker buildx is not installed" <<< "${nobuildx_out}"; then
  pass "--use-ci-images requires docker buildx"
else
  flunk "--use-ci-images requires docker buildx (got: ${nobuildx_out##*$'\n'})"
fi
if grep --quiet ".env file not found" <<< "$(run_prereq Linux --use-ci-images 733)"; then
  pass "--use-ci-images with docker buildx reaches the credential check"
else
  flunk "--use-ci-images with docker buildx reaches the credential check"
fi

# A tool the script calls before its own check would die with a raw shell
# error instead of the check's message. The PATH here mirrors the system
# directories minus the one tool, since /bin and /usr/bin both carry it on a
# merged-/usr host.
for missing in xxd curl; do
  mirror="${tmp}/no-${missing}"
  mkdir -p "${mirror}"
  for dir in /usr/bin /bin; do
    for bin in "${dir}"/*; do
      name="${bin##*/}"
      [[ "${name}" == "${missing}" || -e "${mirror}/${name}" ]] || ln -s "${bin}" "${mirror}/${name}"
    done
  done
  out="$(system_path="${mirror}" run_prereq Linux)"
  if grep --quiet "${missing} is not installed" <<< "${out}"; then
    pass "a host without ${missing} gets the prerequisite message"
  else
    flunk "a host without ${missing} gets the prerequisite message (got: ${out})"
  fi
done

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

# check_bundle <expected-exit> <label> <dir> [expected-output-substring]
check_bundle() {
  local expected="$1" label="$2" dir="$3" want="${4:-}" actual=0 out
  out="$(bash "${verifier}" "${dir}" 720 "${head_sha}" 2>&1)" || actual=$?
  if [[ -n "${want}" ]] && ! grep --quiet --fixed-strings "${want}" <<< "${out}"; then
    flunk "${label}: output lacks '${want}' (got: ${out##*$'\n'})"
  elif [[ "${actual}" -eq "${expected}" ]]; then
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

make_bundle "${tmp}/nohead" 720
rm -f "${tmp}/nohead/head-sha.txt"
check_bundle 1 "a missing head-sha.txt is rejected" "${tmp}/nohead" \
  "head-sha.txt missing from the CI bundle"

make_bundle "${tmp}/nochart" 720
rm -f "${tmp}/nochart/"*.tgz
check_bundle 1 "a missing chart tarball is rejected" "${tmp}/nochart" \
  "chart cloudflare-tunnel-gateway-controller-0.0.0-pr.720-1d.tgz missing"

# The verifier and the PR comment name the two image repositories and the
# workflow builds them; nothing else keeps them in step, and a rename on one
# side would stop --use-ci-images or the comment with a rejection that looks
# like a bad artifact.
mapfile -t bundle_images < <(sed -n 's/.*read_ref [^ ]* \([^)]*\)).*/\1/p' "${verifier}")
if [[ "${#bundle_images[@]}" -ne 2 ]]; then
  flunk "verify-ci-bundle.sh names two image repositories (found ${#bundle_images[@]})"
fi
for image in "${bundle_images[@]}"; do
  if grep --quiet --extended-regexp "^[[:space:]]+image: ${image//./\\.}$" \
    "${repo_root}/.github/workflows/pr.yaml"; then
    pass "pr.yaml builds ${image}, which verify-ci-bundle.sh expects"
  else
    flunk "pr.yaml builds ${image}, which verify-ci-bundle.sh expects"
  fi
  if grep --quiet --extended-regexp "^[[:space:]]+read_ref [a-z]+ ${image//./\\.}$" \
    "${repo_root}/.github/workflows/pr-privileged.yaml"; then
    pass "pr-privileged.yaml accepts ${image}"
  else
    flunk "pr-privileged.yaml accepts ${image}"
  fi
done

# --- wiring ----------------------------------------------------------------
# The verifier only protects anything if the setup script actually runs it.
if grep --quiet "verify-ci-bundle.sh" "${setup}"; then
  pass "conformance-setup.sh invokes the bundle verifier"
else
  flunk "conformance-setup.sh invokes the bundle verifier"
fi

# The pull is the step that depends on a registry outside this repository's
# control, so it must happen before the script deletes the operator's existing
# clusters.
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
  # FAIL_ONCE names a marker file: the first read fails and creates it.
  if [[ -n "${FAIL_ONCE:-}" && ! -f "${FAIL_ONCE}" ]]; then
    : > "${FAIL_ONCE}"
    exit 1
  fi
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
    PULL_CI_IMAGE_RETRY_DELAY=0 \
    bash "${puller}" "ttl.sh/cf-tunnel-gateway-ctrl@sha256:$(printf 'e%.0s' {1..64})" controller:dev "${arch}" \
    >/dev/null 2>&1
}

# pull_err <fixture> <arch> -> stdout+stderr of the puller, exit code ignored
pull_err() {
  PATH="${pull_stub_dir}:/usr/bin:/bin" FIXTURE_INDEX="$1" DOCKER_LOG="${tmp}/docker.log" \
    PULL_CI_IMAGE_RETRY_DELAY=0 \
    bash "${puller}" "ttl.sh/cf-tunnel-gateway-ctrl@sha256:$(printf 'e%.0s' {1..64})" controller:dev "$2" \
    2>&1 || true
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

# One failed read cannot tell an expired image from a dropped connection, so
# the message says what is known rather than asserting the image is gone.
unreadable_err="$(pull_err "${tmp}/absent.json" amd64)"
if grep --quiet "cannot read the image index" <<< "${unreadable_err}"; then
  pass "an unreadable index is reported as unreadable"
else
  flunk "an unreadable index is reported as unreadable (got: ${unreadable_err##*$'\n'})"
fi
if grep --quiet --fixed-strings 'The image is gone from ttl.sh' "${setup}"; then
  flunk "conformance-setup.sh does not claim an unpullable image is gone"
else
  pass "conformance-setup.sh does not claim an unpullable image is gone"
fi

# ttl.sh drops reads transiently, as it does writes; one failure is retried.
rm -f "${tmp}/fail-once"
if FAIL_ONCE="${tmp}/fail-once" run_pull "${tmp}/index.json" amd64; then
  pass "a transient index read failure is retried"
else
  flunk "a transient index read failure is retried"
fi

# Two manifests for the host platform must be refused as such. Without its own
# guard the refusal comes from the digest regex failing on two lines, which a
# later take-the-first refactor would silently turn into a substitution.
index_with_arches "${tmp}/index-dup.json" "${amd64_digest}:amd64" "${arm64_digest}:amd64"
dup_err="$(pull_err "${tmp}/index-dup.json" amd64)"
if grep --quiet "more than one linux/amd64 manifest" <<< "${dup_err}"; then
  pass "an index with two host-platform manifests is refused with its own diagnosis"
else
  flunk "an index with two host-platform manifests is refused with its own diagnosis (got: ${dup_err##*$'\n'})"
fi

# A plain manifest has no `.manifests`, and jq must not hard-error on iterating
# null before the script gets to say what is actually wrong with the reference.
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}' \
  > "${tmp}/index-plain.json"
plain_err="$(PATH="${pull_stub_dir}:/usr/bin:/bin" FIXTURE_INDEX="${tmp}/index-plain.json" \
  DOCKER_LOG="${tmp}/docker.log" bash "${puller}" \
  "ttl.sh/cf-tunnel-gateway-ctrl@sha256:$(printf 'e%.0s' {1..64})" controller:dev amd64 2>&1 || true)"
if grep --quiet "carries no linux/amd64 manifest" <<< "${plain_err}"; then
  pass "a plain manifest gets the script's own diagnosis, not a jq error"
else
  flunk "a plain manifest gets the script's own diagnosis (got: ${plain_err##*$'\n'})"
fi

# A registry error page in place of the index makes jq fail inside the
# assignment, and set -e would end the script there with no message of its own.
printf '<html>502 Bad Gateway</html>' > "${tmp}/index-html.json"
html_err="$(pull_err "${tmp}/index-html.json" amd64)"
if grep --quiet "is not valid JSON" <<< "${html_err}"; then
  pass "a non-JSON index is reported as such"
else
  flunk "a non-JSON index is reported as such (got: ${html_err##*$'\n'})"
fi

# --- probe-ttlsh.sh ------------------------------------------------------------
#
# ttl.sh can keep answering reads while upload initiation hangs with no
# response. The probe decides whether CI pushes at all, so a hang has to read
# as "do not publish" within its own deadline, not hold the job.

prober="${script_dir}/probe-ttlsh.sh"
probe_stub_dir="${tmp}/probestubs"
mkdir -p "${probe_stub_dir}"
# PROBE_CODES lists one response per attempt: an HTTP status, or "hang" for a
# request that never answers (curl's exit 28 after --max-time).
cat > "${probe_stub_dir}/curl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${CURL_LOG}"
attempt="$(grep --count . "${CURL_LOG}")"
read -r -a codes <<< "${PROBE_CODES}"
code="${codes[$((attempt - 1))]:-hang}"
if [[ "${code}" == "hang" ]]; then
  printf '000'
  exit 28
fi
printf '%s' "${code}"
STUB
chmod +x "${probe_stub_dir}/curl"

# run_probe <codes> -> sets probe_out (stdout+stderr) and probe_result (the
# publish= value written to GITHUB_OUTPUT)
run_probe() {
  : > "${tmp}/curl.log"
  : > "${tmp}/probe-output"
  probe_out="$(PATH="${probe_stub_dir}:/usr/bin:/bin" PROBE_CODES="$1" CURL_LOG="${tmp}/curl.log" \
    GITHUB_OUTPUT="${tmp}/probe-output" PROBE_RETRY_DELAY=0 bash "${prober}" 2>&1)" || true
  probe_result="$(sed -n 's/^publish=//p' "${tmp}/probe-output")"
}

run_probe "202"
if [[ "${probe_result}" == "true" ]] && ! grep --quiet '::warning' <<< "${probe_out}"; then
  pass "an accepted upload initiation lets CI publish"
else
  flunk "an accepted upload initiation lets CI publish (publish=${probe_result:-unset}, out: ${probe_out})"
fi
if grep --quiet -- '--max-time' "${tmp}/curl.log" \
  && grep --quiet -- '--request POST' "${tmp}/curl.log" \
  && grep --quiet 'https://ttl.sh/v2/[^ ]*/blobs/uploads/' "${tmp}/curl.log"; then
  pass "the probe is a bounded POST that starts an upload"
else
  flunk "the probe is a bounded POST that starts an upload (curl: $(tr '\n' ';' < "${tmp}/curl.log"))"
fi

run_probe "hang hang"
if [[ "${probe_result}" == "false" ]] && grep --quiet '::warning.*ttl.sh' <<< "${probe_out}"; then
  pass "a hanging upload initiation skips publishing with a warning"
else
  flunk "a hanging upload initiation skips publishing with a warning (publish=${probe_result:-unset}, out: ${probe_out})"
fi

run_probe "hang 202"
if [[ "${probe_result}" == "true" ]]; then
  pass "one hung probe is retried"
else
  flunk "one hung probe is retried (publish=${probe_result:-unset})"
fi

run_probe "503 503"
if [[ "${probe_result}" == "false" ]]; then
  pass "an upload initiation that is refused skips publishing"
else
  flunk "an upload initiation that is refused skips publishing (publish=${probe_result:-unset})"
fi

# The probe only helps if every write to ttl.sh honours it, and a stall that
# starts after the probe passed is bounded only by the step's own timeout.
pr_workflow="${repo_root}/.github/workflows/pr.yaml"
ttlsh_writes=".jobs | to_entries[] | .value as \$job | \$job.steps[]
  | select(((.with.outputs // \"\") | test(\"push=true\")) or ((.run // \"\") | test(\"helm push|imagetools create\")))"
write_count="$(yq "[${ttlsh_writes}] | length" "${pr_workflow}")"
unbounded="$(yq "${ttlsh_writes} | select(.[\"timeout-minutes\"] == null) | .name" "${pr_workflow}")"
ungated="$(yq "${ttlsh_writes} | select(((.if // \"\") + (\$job.if // \"\")) | test(\"needs.registry-probe.outputs.publish == 'true'\") | not) | .name" "${pr_workflow}")"
if [[ "${write_count}" -ge 4 && -z "${unbounded}" ]]; then
  pass "every pr.yaml step that writes to ttl.sh has its own timeout (${write_count} steps)"
else
  flunk "every pr.yaml step that writes to ttl.sh has its own timeout (${write_count} steps; unbounded: ${unbounded//$'\n'/, })"
fi
if [[ "${write_count}" -ge 4 && -z "${ungated}" ]]; then
  pass "every pr.yaml step that writes to ttl.sh is skipped when the probe fails"
else
  flunk "every pr.yaml step that writes to ttl.sh is skipped when the probe fails (ungated: ${ungated//$'\n'/, })"
fi

# A matrix job skipped by its own `if` reports one check under the unexpanded
# `${{ matrix.* }}` name, so the per-leg names branch protection requires
# never appear and the PR stays blocked. Gate the steps instead.
skippable_matrix="$(yq '.jobs | to_entries[] | select(.value.strategy.matrix != null and .value.if != null) | .key' "${pr_workflow}")"
if [[ -z "${skippable_matrix}" ]]; then
  pass "no pr.yaml matrix job can be skipped by a job-level if"
else
  flunk "no pr.yaml matrix job can be skipped by a job-level if (${skippable_matrix//$'\n'/, })"
fi

# --- verify-manifest-children.sh -------------------------------------------
#
# The merge job reads its manifest-list digest back from a run-scoped tag on
# ttl.sh, which has no authentication and whose run id is public for as long as
# the run is. These cases pin that the index that tag resolves to was assembled
# out of the images the job actually pushed, so a substituted index cannot be
# published as this run's reference.
#
# The fixtures follow the production shape: what /tmp/digests names is the
# digest of a per-arch OCI index, and what the published index lists is that
# index's children -- the platform manifest and its attestation -- because
# `imagetools create` flattens index sources.

children="${script_dir}/verify-manifest-children.sh"

amd64_pushed="$(printf '1%.0s' {1..64})"
arm64_pushed="$(printf '2%.0s' {1..64})"
amd64_child="$(printf '3%.0s' {1..64})"
amd64_attest="$(printf '4%.0s' {1..64})"
arm64_child="$(printf '5%.0s' {1..64})"
arm64_attest="$(printf '6%.0s' {1..64})"
plain_pushed="$(printf '7%.0s' {1..64})"

mkdir -p "${tmp}/pushed"
touch "${tmp}/pushed/${amd64_pushed}" "${tmp}/pushed/${arm64_pushed}"

# The script reads each pushed digest back from the registry; the stub serves
# one fixture per digest and fails on anything it was not given.
children_stub_dir="${tmp}/childstubs"
srcidx="${tmp}/srcidx"
mkdir -p "${children_stub_dir}" "${srcidx}"
cat > "${children_stub_dir}/docker" <<'STUB'
#!/usr/bin/env bash
for arg in "$@"; do
  case "${arg}" in
    *@sha256:*)
      fixture="${FIXTURE_DIR}/${arg##*@sha256:}.json"
      [[ -f "${fixture}" ]] || exit 1
      cat "${fixture}"
      exit 0
      ;;
  esac
done
exit 1
STUB
chmod +x "${children_stub_dir}/docker"

# source_index <file> <platform-digest> <arch> <attestation-digest>
source_index() {
  printf '{"manifests":[{"digest":"sha256:%s","platform":{"os":"linux","architecture":"%s"}},{"digest":"sha256:%s","annotations":{"vnd.docker.reference.type":"attestation-manifest"},"platform":{"os":"unknown","architecture":"unknown"}}]}' \
    "$2" "$3" "$4" > "$1"
}

# published_index <file> <digest>...
published_index() {
  local out="$1"; shift
  {
    printf '{"manifests":['
    local sep="" digest
    for digest in "$@"; do
      printf '%s{"digest":"sha256:%s"}' "${sep}" "${digest}"
      sep=","
    done
    printf ']}'
  } > "${out}"
}

source_index "${srcidx}/${amd64_pushed}.json" "${amd64_child}" amd64 "${amd64_attest}"
source_index "${srcidx}/${arm64_pushed}.json" "${arm64_child}" arm64 "${arm64_attest}"

# check_children <expected-exit> <label> <published-index> <digests-dir>
check_children() {
  local expected="$1" label="$2" published="$3" dir="$4" actual=0
  PATH="${children_stub_dir}:/usr/bin:/bin" FIXTURE_DIR="${srcidx}" \
    bash "${children}" ttl.sh/cf-tunnel-gateway-ctrl "${published}" "${dir}" \
    >/dev/null 2>&1 || actual=$?
  if [[ "${actual}" -eq "${expected}" ]]; then
    pass "${label} (exit ${actual})"
  else
    flunk "${label}: expected exit ${expected}, got ${actual}"
  fi
}

published_index "${tmp}/children-ok.json" \
  "${amd64_child}" "${amd64_attest}" "${arm64_child}" "${arm64_attest}"
check_children 0 "an index assembled from this job's images is accepted" \
  "${tmp}/children-ok.json" "${tmp}/pushed"

# The exposure: between the push and the read-back anyone can repoint the tag,
# and the digest read back would then be theirs -- well-formed, digest-pinned,
# and not this build.
published_index "${tmp}/children-sub.json" \
  "$(printf '8%.0s' {1..64})" "$(printf '9%.0s' {1..64})"
check_children 1 "a substituted index is rejected" \
  "${tmp}/children-sub.json" "${tmp}/pushed"

published_index "${tmp}/children-partial.json" "${amd64_child}" "${amd64_attest}"
check_children 1 "an index missing a pushed image's manifests is rejected" \
  "${tmp}/children-partial.json" "${tmp}/pushed"

# Keeping every one of this job's manifests is not enough. `pull-ci-image.sh`
# selects what to deploy by platform, and the verifier constrains no platform,
# so an index that relabels this job's amd64 child and puts a foreign manifest
# in the linux/amd64 slot would pass a subset check and still get a substituted
# image deployed. The published set has to match exactly.
published_index "${tmp}/children-extra.json" \
  "${amd64_child}" "${amd64_attest}" "${arm64_child}" "${arm64_attest}" \
  "$(printf 'e%.0s' {1..64})"
check_children 1 "an index carrying a manifest this job did not push is rejected" \
  "${tmp}/children-extra.json" "${tmp}/pushed"

# Dropping just the attestation is still an index this job did not assemble.
published_index "${tmp}/children-noattest.json" \
  "${amd64_child}" "${amd64_attest}" "${arm64_child}"
check_children 1 "an index missing an attestation manifest is rejected" \
  "${tmp}/children-noattest.json" "${tmp}/pushed"

# A build with provenance off pushes a plain manifest rather than an index, and
# then the pushed digest is itself what has to appear.
mkdir -p "${tmp}/pushed-plain"
touch "${tmp}/pushed-plain/${plain_pushed}"
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}' \
  > "${srcidx}/${plain_pushed}.json"
published_index "${tmp}/children-plain.json" "${plain_pushed}"
check_children 0 "a plain manifest is matched by its own digest" \
  "${tmp}/children-plain.json" "${tmp}/pushed-plain"
check_children 1 "a plain manifest absent from the index is rejected" \
  "${tmp}/children-ok.json" "${tmp}/pushed-plain"

# A pushed digest the registry will not serve must stop the job rather than be
# skipped: a source that cannot be read is a source that cannot be vouched for.
unreadable_pushed="$(printf 'b%.0s' {1..64})"
mkdir -p "${tmp}/pushed-unreadable"
touch "${tmp}/pushed-unreadable/${unreadable_pushed}"
check_children 1 "a pushed digest that cannot be read is rejected" \
  "${tmp}/children-ok.json" "${tmp}/pushed-unreadable"

# ...and specifically because the read failed, not because the digest happened
# to be absent. With the source digest itself listed, only the failed read is
# left to reject it.
published_index "${tmp}/children-unreadable.json" "${unreadable_pushed}"
check_children 1 "an unreadable source is rejected even when its own digest is listed" \
  "${tmp}/children-unreadable.json" "${tmp}/pushed-unreadable"

# An empty directory would make the loop vacuously true, and every index would
# pass.
mkdir -p "${tmp}/nodigests"
check_children 1 "an empty digest set is rejected" \
  "${tmp}/children-ok.json" "${tmp}/nodigests"

# ...including against an empty index, where the two sets would otherwise
# compare equal and the comparison alone would wave it through.
published_index "${tmp}/children-empty.json"
check_children 1 "an empty digest set is rejected against an empty index" \
  "${tmp}/children-empty.json" "${tmp}/nodigests"

# The exit code alone does not pin that guard: with it removed the empty set
# still fails, but as a pipefail from `grep` finding nothing. What the guard is
# worth is the operator being told which of the two inputs was empty.
empty_err="$(PATH="${children_stub_dir}:/usr/bin:/bin" FIXTURE_DIR="${srcidx}" \
  bash "${children}" ttl.sh/cf-tunnel-gateway-ctrl \
  "${tmp}/children-empty.json" "${tmp}/nodigests" 2>&1 || true)"
if grep --quiet "no pushed digests to check" <<< "${empty_err}"; then
  pass "an empty digest set is refused by name, not by a pipeline error"
else
  flunk "an empty digest set is refused by name (got: ${empty_err##*$'\n'})"
fi

check_children 1 "an unreadable published index is rejected" \
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

# ...and the suite only guards them if editing them triggers it. Both paths
# blocks, since the pull_request one gates the PR and the push one gates master.
for guarded in hack/verify-manifest-children.sh .github/workflows/pr.yaml \
  .github/workflows/pr-privileged.yaml; do
  occurrences="$(grep --count --fixed-strings "      - ${guarded}" \
    "${repo_root}/.github/workflows/scripts.yaml" || true)"
  if [[ "${occurrences}" -eq 2 ]]; then
    pass "scripts.yaml runs on changes to ${guarded}"
  else
    flunk "scripts.yaml runs on changes to ${guarded} (${occurrences} of 2 paths blocks)"
  fi
done

# --- find-ci-run.sh ---------------------------------------------------------
#
# This is the trust root of --use-ci-images: whatever run it names has its
# artifacts deployed into a cluster holding live Cloudflare credentials. Each
# filter is pinned separately, because dropping any one of them silently widens
# what is accepted rather than breaking anything visible.

finder="${script_dir}/find-ci-run.sh"

gh_stub_dir="${tmp}/ghstubs"
mkdir -p "${gh_stub_dir}"
cat > "${gh_stub_dir}/gh" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  pr)  [[ -n "${FIXTURE_HEAD:-}" ]] || exit 1; echo "${FIXTURE_HEAD}" ;;
  api) if [[ "$2" == */artifacts* ]]; then cat "${FIXTURE_ARTIFACTS}"; else cat "${FIXTURE_RUNS}"; fi ;;
  *)   exit 1 ;;
esac
STUB
chmod +x "${gh_stub_dir}/gh"

ci_head="$(printf 'a%.0s' {1..40})"
ci_old_head="$(printf 'f%.0s' {1..40})"

# runs_fixture <file> <entry>...  where entry is name|event|conclusion|sha|created|id
runs_fixture() {
  local out="$1"; shift
  {
    printf '{"workflow_runs":['
    local sep="" e
    for e in "$@"; do
      IFS='|' read -r name event concl sha created id <<< "${e}"
      printf '%s{"name":"%s","event":"%s","conclusion":"%s","head_sha":"%s","created_at":"%s","id":%s}' \
        "${sep}" "${name}" "${event}" "${concl}" "${sha}" "${created}" "${id}"
      sep=","
    done
    printf ']}'
  } > "${out}"
}

# check_finder <expected-exit> <label> <runs-fixture> [expected-stdout-substring]
check_finder() {
  local expected="$1" label="$2" fixture="$3" want="${4:-}" actual=0 out
  out="$(PATH="${gh_stub_dir}:/usr/bin:/bin" FIXTURE_HEAD="${ci_head}" FIXTURE_RUNS="${fixture}" \
    FIXTURE_ARTIFACTS="${artifacts_fixture}" bash "${finder}" 733 2>&1)" || actual=$?
  if [[ "${actual}" -ne "${expected}" ]]; then
    flunk "${label}: expected exit ${expected}, got ${actual}"
  elif [[ -n "${want}" ]] && ! grep --quiet --fixed-strings "${want}" <<< "${out}"; then
    flunk "${label}: output lacks '${want}' (got: ${out##*$'\n'})"
  else
    pass "${label} (exit ${actual})"
  fi
}

good_run="PR Checks and Build|pull_request|success|${ci_head}|2026-09-05T10:00:00Z|111"

printf '{"total_count":1,"artifacts":[{"name":"image-ref-controller","expired":false}]}' > "${tmp}/artifacts-published.json"
printf '{"total_count":0,"artifacts":[]}' > "${tmp}/artifacts-none.json"
printf '{"message":"Server Error"}' > "${tmp}/artifacts-malformed.json"
artifacts_fixture="${tmp}/artifacts-published.json"

runs_fixture "${tmp}/runs-ok.json" "${good_run}"
check_finder 0 "a successful pull_request run at this head is accepted" \
  "${tmp}/runs-ok.json" "run_id=111"

# A failed run may have published only some of its artifacts.
runs_fixture "${tmp}/runs-failed.json" \
  "PR Checks and Build|pull_request|failure|${ci_head}|2026-09-05T10:00:00Z|111"
check_finder 1 "a failed run is refused" "${tmp}/runs-failed.json" "No successful"

# Only the pull_request event builds the PR's own code.
runs_fixture "${tmp}/runs-dispatch.json" \
  "PR Checks and Build|workflow_dispatch|success|${ci_head}|2026-09-05T10:00:00Z|111"
check_finder 1 "a workflow_dispatch run is refused" "${tmp}/runs-dispatch.json" "No successful"

runs_fixture "${tmp}/runs-push.json" \
  "PR Checks and Build|push|success|${ci_head}|2026-09-05T10:00:00Z|111"
check_finder 1 "a push run is refused" "${tmp}/runs-push.json" "No successful"

# A run for an older head built a different diff than the one under review.
runs_fixture "${tmp}/runs-oldhead.json" \
  "PR Checks and Build|pull_request|success|${ci_old_head}|2026-09-05T10:00:00Z|111"
check_finder 1 "a run for an earlier head is refused" "${tmp}/runs-oldhead.json" "No successful"

# Another workflow's run carries none of the artifacts this flag needs.
runs_fixture "${tmp}/runs-otherwf.json" \
  "Scripts|pull_request|success|${ci_head}|2026-09-05T10:00:00Z|111"
check_finder 1 "another workflow's run is refused" "${tmp}/runs-otherwf.json" "No successful"

# Re-running CI must win over the run it replaced.
runs_fixture "${tmp}/runs-two.json" \
  "PR Checks and Build|pull_request|success|${ci_head}|2026-09-05T10:00:00Z|111" \
  "PR Checks and Build|pull_request|success|${ci_head}|2026-09-05T12:00:00Z|222"
check_finder 0 "the newest successful run wins" "${tmp}/runs-two.json" "run_id=222"

runs_fixture "${tmp}/runs-none.json"
check_finder 1 "an empty run list is refused" "${tmp}/runs-none.json" "No successful"

# A run that skipped publishing because ttl.sh refused uploads succeeds without
# images; say so instead of reporting its artifacts as expired.
artifacts_fixture="${tmp}/artifacts-none.json"
check_finder 1 "a run that published no images is refused as such" "${tmp}/runs-ok.json" "published no images"
artifacts_fixture="${tmp}/artifacts-malformed.json"
check_finder 1 "an artifacts response without a count is refused" "${tmp}/runs-ok.json" "no artifact count"
artifacts_fixture="${tmp}/artifacts-published.json"

# The head the artifacts are bound to is the one the caller must verify against.
check_finder 0 "the resolved head is reported to the caller" \
  "${tmp}/runs-ok.json" "head_sha=${ci_head}"

if [[ "$(PATH="${gh_stub_dir}:/usr/bin:/bin" FIXTURE_HEAD="${ci_head}" \
  FIXTURE_RUNS="${tmp}/runs-ok.json" bash "${finder}" not-a-number 2>&1 || true)" == *"must be numeric"* ]]; then
  pass "a non-numeric PR number is refused"
else
  flunk "a non-numeric PR number is refused"
fi

# pr-number.txt is produced by the fork's own pr.yaml, so every privileged
# step that writes to a PR must first confirm the PR's head is the commit the
# run built. The label step is gated indirectly, through first_build.
match_steps="$(grep --count 'name: Check the PR is the one this run built' \
  "${repo_root}/.github/workflows/pr-privileged.yaml" || true)"
if [[ "${match_steps}" -eq 2 ]]; then
  pass "both pr-privileged.yaml jobs check the PR head against the run"
else
  flunk "both pr-privileged.yaml jobs check the PR head against the run (${match_steps} of 2)"
fi
for gated in "Download SARIF artifact" "Upload SARIF to GitHub Security" "Check if first build" \
  "Check whether the run published" "Remove Container Available label" "Download image references" "Read and validate image references" "Comment on PR"; do
  if grep --after-context=3 --fixed-strings -- "- name: ${gated}" \
    "${repo_root}/.github/workflows/pr-privileged.yaml" \
    | grep --quiet --fixed-strings "steps.match.outputs.current == 'true'"; then
    pass "pr-privileged.yaml '${gated}' runs only for the PR this run built"
  else
    flunk "pr-privileged.yaml '${gated}' runs only for the PR this run built"
  fi
done

# The finder and the privileged workflow select runs by the workflow's display
# name, which has to stay equal to pr.yaml's own `name:`.
pr_workflow_name="$(sed -n 's/^name: //p' "${repo_root}/.github/workflows/pr.yaml")"
if grep --quiet --fixed-strings "select(.name == \"${pr_workflow_name}\")" "${finder}"; then
  pass "find-ci-run.sh selects runs of '${pr_workflow_name}'"
else
  flunk "find-ci-run.sh selects runs of '${pr_workflow_name}'"
fi
if grep --quiet --fixed-strings "workflows: [\"${pr_workflow_name}\"]" \
  "${repo_root}/.github/workflows/pr-privileged.yaml"; then
  pass "pr-privileged.yaml triggers on '${pr_workflow_name}'"
else
  flunk "pr-privileged.yaml triggers on '${pr_workflow_name}'"
fi

# The finder only protects anything if the setup script uses it.
if grep --quiet --extended-regexp '\$\{REPO_ROOT\}/hack/find-ci-run\.sh' "${setup}"; then
  pass "conformance-setup.sh resolves the run through the finder"
else
  flunk "conformance-setup.sh resolves the run through the finder"
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "conformance-setup tests FAILED"
  exit 1
fi
echo "conformance-setup tests passed"
