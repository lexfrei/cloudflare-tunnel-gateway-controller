#!/usr/bin/env bash
# lint-action-shell.sh — shellcheck the inline shell embedded in composite
# GitHub Actions.
#
# actionlint lints workflow YAML and the inline `run:` blocks of workflows, but
# it does NOT lint composite `action.yml` files, and a wrapped action's
# `command:` input is a YAML string rather than a `run:` block — so the shell
# inside .github/actions/**/action.yml is invisible to both actionlint and a
# `shellcheck hack/*.sh` pass. This script closes that gap: it extracts every
# `run:` and `command:` block from each composite action and runs shellcheck
# on it.
#
# Requires: yq (mikefarah), shellcheck — both present on GitHub ubuntu runners.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

status=0
found=0

while IFS= read -r action; do
  count="$(yq '[.runs.steps[] | select(.run or .with.command)] | length' "${action}")"
  for ((i = 0; i < count; i++)); do
    found=1
    echo "==> shellcheck ${action} (shell block #${i})"
    if ! yq "[.runs.steps[] | select(.run or .with.command)] | .[${i}] | (.run // .with.command)" "${action}" \
        | shellcheck --shell=bash -; then
      status=1
    fi
  done
done < <(find .github/actions -type f \( -name action.yml -o -name action.yaml \) | sort)

if [[ "${found}" -eq 0 ]]; then
  echo "No composite-action shell blocks found."
fi

exit "${status}"
