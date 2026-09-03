#!/usr/bin/env bash
# conformance-setup_test.sh — tests for conformance-setup.sh.
#
# Pins the colima prerequisite as a macOS requirement rather than a "not in CI"
# requirement, so a Linux host with a native docker daemon gets past the tool
# check. Plain bash and temp dirs, no test framework.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
setup="${script_dir}/conformance-setup.sh"

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

if [[ "${fail}" -ne 0 ]]; then
  echo "conformance-setup tests FAILED"
  exit 1
fi
echo "conformance-setup tests passed"
