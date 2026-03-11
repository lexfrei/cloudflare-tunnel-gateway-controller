#!/usr/bin/env bash
# conformance-teardown.sh — Clean up conformance test environment.
#
# Deletes all v2-test-* kind clusters used for conformance testing.
#
# Usage:
#   ./hack/conformance-teardown.sh

set -euo pipefail

info() { echo "==> $*"; }

deleted=0
for cluster in $(kind get clusters 2>/dev/null | grep "^v2-test" || true); do
  info "Deleting kind cluster '${cluster}'..."
  kind delete cluster --name "${cluster}"
  deleted=$((deleted + 1))
done

if [[ "${deleted}" -eq 0 ]]; then
  info "No v2-test-* kind clusters found, nothing to do."
else
  info "Done. Deleted ${deleted} cluster(s)."
fi
