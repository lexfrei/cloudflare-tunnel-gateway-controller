#!/usr/bin/env bash
# probe-ttlsh.sh — decide whether CI should push to ttl.sh at all.
#
# ttl.sh can keep answering reads while starting an upload hangs with no
# response, for hours; every push then sits until its job times out. Starting
# an upload is the exact call that hangs, so that is what this probes, with a
# deadline. The session it opens is never used and expires on its own.
#
# Writes publish=true|false to $GITHUB_OUTPUT; false also emits a warning
# annotation.

set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must name the step output file}"

url="https://ttl.sh/v2/cf-tunnel-gateway-probe/blobs/uploads/"
code=""
for attempt in 1 2; do
  code="$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --max-time 15 --request POST "${url}" || true)"
  [[ "${code}" == "202" ]] && break
  [[ "${attempt}" -lt 2 ]] && sleep "${PROBE_RETRY_DELAY:-5}"
done

if [[ "${code}" == "202" ]]; then
  echo "publish=true" >> "${GITHUB_OUTPUT}"
else
  echo "::warning::ttl.sh is not accepting uploads (POST ${url} answered '${code:-nothing}'); images and the chart are built but not published"
  echo "publish=false" >> "${GITHUB_OUTPUT}"
fi
