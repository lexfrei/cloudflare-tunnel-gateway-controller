#!/usr/bin/env bash
# check-doc-links.sh — fail if any tracked markdown links to the versioned docs
# site (https://cf.k8s.lex.la) without the required /latest/ (or /vX.Y/) prefix.
#
# The site is versioned with mike, so only /latest/<page>/ and /vX.Y/<page>/
# resolve; a versionless https://cf.k8s.lex.la/<page>/ hits GitHub Pages'
# fallback and 404s. README.md and the other repo-root files are not part of
# the mkdocs build, so their links to the site must be absolute and carry
# /latest/. mkdocs build --strict validates relative/internal links but not
# http(s) absolutes, which is why this guard exists.
#
# The CRD apiVersion (cf.k8s.lex.la/v1alpha1) and the controllerName
# (cf.k8s.lex.la/tunnel-*) are identifiers, never written with an https://
# scheme, so anchoring the match on https://cf.k8s.lex.la/ already excludes
# them.
#
# Usage:
#   check-doc-links.sh [FILE...]
# With no arguments, scans every tracked text file (vendor and this guard's own
# test fixtures excluded) -- doc links also live in non-.md sources such as the
# Helm NOTES.txt that is printed after `helm install`. Explicit file arguments
# are scanned as-is (used by the test).
set -euo pipefail

# A docs-site URL whose first path segment starts with a letter — i.e. a doc
# page (gateway-api, operations, ...) or the latest / vX alias. The match stops
# at whitespace or a markdown/HTML delimiter. The bare host (no trailing path)
# and digit-versioned paths (/3.0.0/) do not match.
url_re='https?://cf\.k8s\.lex\.la/[A-Za-z][^[:space:])">`]*'
# A link is allowed when its FIRST path segment is the latest alias or a version
# directory. The release workflow strips the leading v
# (VERSION="${GITHUB_REF_NAME#v}") and mike publishes semver dirs (3.0.0), so
# /latest/ and a leading digit cover every real version path; v?[0-9] also
# tolerates a hand-written /vX/ link rather than flag it. Anchored to the
# file:line: prefix that grep -Hn prepends so the check binds to the segment
# right after the host, not to a "cf.k8s.lex.la/latest" that appears later in
# the path or query of an otherwise-versionless link.
allowed_re='^[^:]+:[0-9]+:https?://cf\.k8s\.lex\.la/(latest|v?[0-9])'

if [[ "$#" -gt 0 ]]; then
  files=("$@")
else
  # check-doc-links_test.sh is excluded: it embeds versionless links on purpose
  # as test fixtures, which would otherwise self-trip this guard.
  mapfile -t files < <(git ls-files ':!vendor/**' ':!hack/check-doc-links_test.sh')
fi

# grep exits 1 when nothing matches; that is success here, so swallow it.
# -I skips binary files (images, etc.) now that the scan is not limited to *.md.
matches="$(grep -oEHnI "${url_re}" -- "${files[@]}" || true)"

offenders=""
if [[ -n "${matches}" ]]; then
  offenders="$(printf '%s\n' "${matches}" | grep -Ev "${allowed_re}" || true)"
fi

if [[ -n "${offenders}" ]]; then
  {
    echo "ERROR: documentation links to https://cf.k8s.lex.la must carry the"
    echo "       /latest/ version prefix — the site is versioned with mike, so a"
    echo "       versionless deep link 404s. Offending links:"
    echo ""
    printf '%s\n' "${offenders}"
  } >&2
  exit 1
fi

echo "OK: no versionless cf.k8s.lex.la documentation links found"
