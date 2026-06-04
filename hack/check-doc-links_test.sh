#!/usr/bin/env bash
# check-doc-links_test.sh — tests for check-doc-links.sh.
#
# Pins the guard's behaviour: a versionless cf.k8s.lex.la documentation link
# fails, while /latest/ links, the bare host, and the CRD apiVersion /
# controllerName identifiers pass. Plain bash + a temp dir, no test framework.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="${script_dir}/check-doc-links.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail=0

# check <expected-exit> <label> <file>
check() {
  local expected="$1" label="$2" file="$3" actual=0
  bash "${guard}" "${file}" >/dev/null 2>&1 || actual=$?
  if [[ "${actual}" -eq "${expected}" ]]; then
    echo "ok   - ${label} (exit ${actual})"
  else
    echo "FAIL - ${label}: expected exit ${expected}, got ${actual}"
    fail=1
  fi
}

# check_repo <expected-exit> <label> <relpath> <content>
# Stages <content> at <relpath> in a throwaway git repo and runs the guard in
# its default (no-argument) mode, exercising the git ls-files file scope -- the
# part that decides which files are even looked at. A bad link in a non-.md
# source like a Helm NOTES.txt must be in scope.
check_repo() {
  local expected="$1" label="$2" relpath="$3" content="$4" actual=0 repo
  repo="$(mktemp -d)"
  git -C "${repo}" init --quiet
  mkdir -p "${repo}/$(dirname "${relpath}")"
  printf '%s\n' "${content}" > "${repo}/${relpath}"
  git -C "${repo}" add --all
  ( cd "${repo}" && bash "${guard}" ) >/dev/null 2>&1 || actual=$?
  rm -rf "${repo}"
  if [[ "${actual}" -eq "${expected}" ]]; then
    echo "ok   - ${label} (exit ${actual})"
  else
    echo "FAIL - ${label}: expected exit ${expected}, got ${actual}"
    fail=1
  fi
}

printf 'See [Limitations](https://cf.k8s.lex.la/gateway-api/limitations/).\n' \
  > "${tmp}/bad.md"
printf 'See [Limitations](https://cf.k8s.lex.la/latest/gateway-api/limitations/).\n' \
  > "${tmp}/good_latest.md"
printf 'Docs: [home](https://cf.k8s.lex.la) and <https://cf.k8s.lex.la>.\n' \
  > "${tmp}/good_root.md"
printf 'apiVersion: cf.k8s.lex.la/v1alpha1\ncontrollerName: cf.k8s.lex.la/tunnel-controller\n' \
  > "${tmp}/good_identifiers.md"
printf 'Links: [a](https://cf.k8s.lex.la/latest/guides/) [b](https://cf.k8s.lex.la/operations/).\n' \
  > "${tmp}/bad_mixed.md"
# Versionless links whose path or query happens to contain the substring
# "cf.k8s.lex.la/latest" later on must still be caught: the version check is
# anchored to the URL's first path segment, not matched anywhere on the line.
printf 'Sneaky: [x](https://cf.k8s.lex.la/guides/cf.k8s.lex.la/latest/x).\n' \
  > "${tmp}/bad_sneaky_path.md"
printf 'Query: [x](https://cf.k8s.lex.la/operations/?ref=cf.k8s.lex.la/latest).\n' \
  > "${tmp}/bad_sneaky_query.md"

check 1 "versionless doc link fails"         "${tmp}/bad.md"
check 0 "/latest/ link passes"               "${tmp}/good_latest.md"
check 0 "bare host passes"                   "${tmp}/good_root.md"
check 0 "apiVersion / controllerName pass"   "${tmp}/good_identifiers.md"
check 1 "mixed line: bad link still caught"  "${tmp}/bad_mixed.md"
check 1 "downstream /latest/ in path caught" "${tmp}/bad_sneaky_path.md"
check 1 "downstream /latest/ in query caught" "${tmp}/bad_sneaky_query.md"

# Default-scope coverage: a Helm NOTES.txt (a non-.md, user-facing doc-link
# source) must be scanned, not just *.md files.
check_repo 1 "default scope catches a versionless link in NOTES.txt" \
  "charts/x/templates/NOTES.txt" \
  "  Troubleshooting: https://cf.k8s.lex.la/operations/troubleshooting/"
check_repo 0 "default scope passes a /latest/ link in NOTES.txt" \
  "charts/x/templates/NOTES.txt" \
  "  Troubleshooting: https://cf.k8s.lex.la/latest/operations/troubleshooting/"

if [[ "${fail}" -ne 0 ]]; then
  echo "doc-link guard tests FAILED"
  exit 1
fi
echo "doc-link guard tests passed"
