#!/usr/bin/env bash
# find-ci-run.sh — pick the CI run whose artifacts may be deployed for a PR.
#
# This is the trust root of `conformance-setup.sh --use-ci-images`: whatever run
# this names has its artifacts unpacked and deployed into a cluster that holds
# live Cloudflare credentials. Every filter is load-bearing, and dropping one
# widens what is accepted without breaking anything visible:
#
#   name         another workflow's run carries none of these artifacts
#   event        only `pull_request` builds the PR's own code
#   conclusion   a failed run may have published only some of its artifacts
#   head_sha     a run for an older head built a different diff than the one
#                under review
#
# Newest wins, so re-running CI supersedes the run it replaced.
#
# Usage: find-ci-run.sh <pr-number>
# Prints `key=value` lines for the caller on success.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: $0 <pr-number>"

pr_number="$1"
[[ "${pr_number}" =~ ^[0-9]+$ ]] || die "PR number must be numeric, got '${pr_number}'"

head_sha="$(gh pr view "${pr_number}" --json headRefOid --jq '.headRefOid')" \
  || die "Cannot read PR #${pr_number} (is gh authenticated for this repo?)"
[[ -n "${head_sha}" ]] || die "PR #${pr_number} reports no head commit"

run_id="$(gh api "repos/{owner}/{repo}/actions/runs?head_sha=${head_sha}&per_page=100" \
  | jq --raw-output --arg sha "${head_sha}" '
      [ .workflow_runs[]
        | select(.name == "PR Checks and Build")
        | select(.event == "pull_request")
        | select(.conclusion == "success")
        | select(.head_sha == $sha)
      ] | sort_by(.created_at) | last | .id // empty')" \
  || die "Querying workflow runs for head ${head_sha} failed (gh api actions/runs)"

[[ -n "${run_id}" ]] \
  || die "No successful 'PR Checks and Build' run for PR #${pr_number} at head ${head_sha}. Re-run its CI; a run for an earlier head is not accepted."

echo "head_sha=${head_sha}"
echo "run_id=${run_id}"
