#!/usr/bin/env bash
# release-changelog.sh — turn commit subjects into release changelog entries.
#
# Reads Conventional Commit subjects on stdin, one per line, and prints one
# entry per user-facing subject. A `!` before the colon marks a breaking
# change, which is kept and labelled as such.
#
# Usage:
#   git log v1.0.0..HEAD --format=%s --no-merges | hack/release-changelog.sh markdown
#     -> "- <emoji> [**Breaking:** ][scope: ]description" lines for the release notes
#   git log v1.0.0..HEAD --format=%s --no-merges | hack/release-changelog.sh artifacthub
#     -> "<kind>\t[Breaking: ][scope: ]description" lines for the chart's
#        artifacthub.io/changes annotation
set -euo pipefail

format="${1:-}"
case "${format}" in
  markdown | artifacthub) ;;
  *)
    echo "usage: $0 markdown|artifacthub < subjects" >&2
    exit 2
    ;;
esac

pattern='^(feat|fix|security|refactor|docs|perf|build)(\(([^)]+)\))?(!)?: (.+)$'

while IFS= read -r subject; do
  [[ "${subject}" =~ ${pattern} ]] || continue
  type="${BASH_REMATCH[1]}"
  scope="${BASH_REMATCH[3]}"
  breaking="${BASH_REMATCH[4]}"
  text="${BASH_REMATCH[5]}"
  [[ -n "${scope}" ]] && text="${scope}: ${text}"

  if [[ "${format}" == markdown ]]; then
    case "${type}" in
      feat) emoji="✨" ;;
      fix) emoji="🐛" ;;
      security) emoji="🔒" ;;
      *) emoji="🔄" ;;
    esac
    [[ -n "${breaking}" ]] && text="**Breaking:** ${text}"
    printf -- '- %s %s\n' "${emoji}" "${text}"
  else
    case "${type}" in
      feat) kind="added" ;;
      fix) kind="fixed" ;;
      security) kind="security" ;;
      *) kind="changed" ;;
    esac
    [[ -n "${breaking}" ]] && text="Breaking: ${text}"
    printf '%s\t%s\n' "${kind}" "${text}"
  fi
done
