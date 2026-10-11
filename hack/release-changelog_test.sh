#!/usr/bin/env bash
# release-changelog_test.sh — tests for release-changelog.sh.
#
# Pins which commit subjects reach the release changelog and how a
# breaking-change subject (`type!:` / `type(scope)!:`) is rendered in both the
# GitHub release notes and the Artifact Hub annotation. Plain bash, no test
# framework.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
changelog="${script_dir}/release-changelog.sh"

fail=0

# check <label> <format> <subjects> <expected-output>
check() {
  local label="$1" format="$2" subjects="$3" expected="$4" actual status=0
  actual="$(printf '%s\n' "${subjects}" | bash "${changelog}" "${format}" 2>&1)" || status=$?
  if [[ "${status}" -eq 0 && "${actual}" == "${expected}" ]]; then
    echo "ok   - ${label}"
  else
    echo "FAIL - ${label} (exit ${status})"
    echo "  expected: $(printf '%q' "${expected}")"
    echo "  actual:   $(printf '%q' "${actual}")"
    fail=1
  fi
}

subjects='feat(proxy): add a thing (#1)
fix(controller)!: report status the spec requires (#979)
fix!: close breaking-but-correct defaults (#876)
chore(deps): bump something
ci(release)!: rework the pipeline
security: patch a hole
docs(readme): reword

refactor(helm)!: rename a value
not a conventional subject'

check "markdown keeps user-facing types and marks breaking ones" markdown "${subjects}" \
'- ✨ proxy: add a thing (#1)
- 🐛 **Breaking:** controller: report status the spec requires (#979)
- 🐛 **Breaking:** close breaking-but-correct defaults (#876)
- 🔒 patch a hole
- 🔄 readme: reword
- 🔄 **Breaking:** helm: rename a value'

check "artifacthub emits kind<TAB>description and marks breaking ones" artifacthub "${subjects}" \
"added	proxy: add a thing (#1)
fixed	Breaking: controller: report status the spec requires (#979)
fixed	Breaking: close breaking-but-correct defaults (#876)
security	patch a hole
changed	readme: reword
changed	Breaking: helm: rename a value"

check "no user-facing subjects produce no output" markdown 'chore: tidy
ci: tweak' ''

check "a bang outside the type position is not a breaking marker" markdown \
'feat(proxy): support ! in descriptions' \
'- ✨ proxy: support ! in descriptions'

actual_status=0
printf 'feat: x\n' | bash "${changelog}" bogus >/dev/null 2>&1 || actual_status=$?
if [[ "${actual_status}" -ne 0 ]]; then
  echo "ok   - unknown format is refused"
else
  echo "FAIL - unknown format is refused (exit 0)"
  fail=1
fi

exit "${fail}"
