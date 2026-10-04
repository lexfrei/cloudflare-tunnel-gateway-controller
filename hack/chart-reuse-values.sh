#!/usr/bin/env bash
# chart-reuse-values.sh — render the chart the way `helm upgrade --reuse-values`
# does from the released values.yaml files under tests/fixtures.
#
# --reuse-values hands the new templates the previous release's values in place
# of the chart defaults, so a key added since that release is nil rather than
# defaulted. helm-unittest cannot reproduce that: its `values:` files merge over
# the defaults. Swapping a copy of the chart's values.yaml for an old release's
# file can, without anyone listing the keys that release lacked.
#
# Each fixture renders twice: with only the required values, and with every
# boolean the current values.yaml defines switched on, so templates
# behind a toggle read their keys too.
#
# Fixtures come from `git show <tag>:charts/cloudflare-tunnel-gateway-controller/values.yaml`.
#
# Usage: chart-reuse-values.sh [chart-dir]

set -euo pipefail

chart="${1:-charts/cloudflare-tunnel-gateway-controller}"
fixtures=("${chart}"/tests/fixtures/values-*.yaml)
[[ -e "${fixtures[0]}" ]] || { echo "no fixtures under ${chart}/tests/fixtures" >&2; exit 1; }

required=(
  --set proxy.tunnelTokenSecretRef.name=tunnel-token
  --set gatewayClassConfig.cloudflareCredentialsSecretRef.name=cloudflare-credentials
  --set gatewayClassConfig.tunnelID=550e8400-e29b-41d4-a716-446655440000
)
mapfile -t toggles < <(yq '.. | select(tag == "!!bool") | "--set=" + (path | join(".")) + "=true"' "${chart}/values.yaml")
[[ ${#toggles[@]} -gt 0 ]] || { echo "found no toggles in ${chart}/values.yaml" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failed=0
for fixture in "${fixtures[@]}"; do
  name="$(basename "${fixture}" .yaml)"
  copy="${work}/${name}"
  mkdir -p "${copy}"
  cp -R "${chart}/." "${copy}/"
  cp "${fixture}" "${copy}/values.yaml"
  for mode in minimal toggles; do
    args=("${required[@]}")
    [[ "${mode}" == toggles ]] && args+=("${toggles[@]}")
    if helm template reuse "${copy}" "${args[@]}" >/dev/null; then
      echo "ok   ${name} (${mode})"
    else
      echo "FAIL ${name} (${mode})" >&2
      failed=1
    fi
  done
done
exit "${failed}"
