#!/usr/bin/env bash
# conformance-setup.sh — Reproducible setup for Gateway API conformance tests.
#
# This script:
#   1. Ensures colima is running
#   2. Deletes old v2-test-* kind clusters
#   3. Creates a fresh kind cluster with random suffix
#   4. Installs Gateway API CRDs (experimental channel)
#   5. Builds controller + proxy images          (skipped in --use-ci-images mode)
#   6. Loads images into kind                     (skipped in --use-ci-images mode)
#   7. Creates secrets from .env
#   8. Deploys controller via helm               (local chart, or PR's ttl.sh chart)
#   9. Waits for readiness
#  10. Optionally runs conformance tests
#
# Usage:
#   ./hack/conformance-setup.sh                  # full setup (fresh cluster, local build)
#   ./hack/conformance-setup.sh --test           # setup + run tests
#   ./hack/conformance-setup.sh --skip-build     # skip image build (reuse existing)
#   ./hack/conformance-setup.sh --use-ci-images N  # deploy PR #N's published ttl.sh
#                                                  # chart+images (no local build)
#
# Prerequisites:
#   - .env file in repo root with: CF_API_TOKEN, CF_ACCOUNT_ID, V2_TUNNEL_ID,
#     V2_TUNNEL_TOKEN, V2_TUNNEL_HOSTNAME (the edge hostname routing to the tunnel)
#   - colima, docker, kind, helm, kubectl, go installed

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# --- Configuration ---
CLUSTER_SUFFIX="$(head -c 4 /dev/urandom | xxd -plain)"
CLUSTER_NAME="v2-test-${CLUSTER_SUFFIX}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="cloudflare-tunnel-system"
TEST_NAMESPACE="conformance-test"
RELEASE_NAME="cftunnel"
CONTROLLER_IMAGE="controller:dev"
PROXY_IMAGE="proxy:dev"
GATEWAY_API_VERSION="v1.5.1"

# --- Flags ---
RUN_TESTS=false
SKIP_BUILD=false
CI_PR_NUMBER=""

# --- Helpers ---
info()  { echo "==> $*"; }
warn()  { echo "WARN: $*" >&2; }
die()   { echo "ERROR: $*" >&2; exit 1; }

check_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not installed"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --test) RUN_TESTS=true ;;
    --skip-build) SKIP_BUILD=true ;;
    --use-ci-images)
      shift
      [[ $# -gt 0 ]] || die "--use-ci-images requires a PR number"
      CI_PR_NUMBER="$1"
      [[ "${CI_PR_NUMBER}" =~ ^[0-9]+$ ]] || die "--use-ci-images PR number must be numeric, got '${CI_PR_NUMBER}'"
      ;;
    *) die "Unknown flag: $1" ;;
  esac
  shift
done

# --use-ci-images is itself a no-build path; combining with --skip-build is contradictory.
if [[ -n "${CI_PR_NUMBER}" && "${SKIP_BUILD}" == "true" ]]; then
  die "--use-ci-images and --skip-build are mutually exclusive"
fi

# --- Pre-flight checks ---
info "Checking prerequisites..."
for tool in colima docker kind helm kubectl go; do
  check_tool "${tool}"
done

# --- Load .env ---
ENV_FILE="${REPO_ROOT}/.env"
if [[ ! -f "${ENV_FILE}" ]]; then
  die ".env file not found at ${ENV_FILE}. See .env.example or MEMORY.md for required variables."
fi

# shellcheck source=/dev/null
source "${ENV_FILE}"

for var in CF_API_TOKEN CF_ACCOUNT_ID V2_TUNNEL_ID V2_TUNNEL_TOKEN V2_TUNNEL_HOSTNAME; do
  if [[ -z "${!var:-}" ]]; then
    die "Required variable ${var} is not set in .env (see .env.example)"
  fi
done

# --- Pre-flight: validate the Cloudflare token against the real tunnel endpoint ---
# A non-empty token is not enough: an expired/invalid token deploys fine but then
# 401s on every cfd_tunnel/.../configurations call, Gateways never reach
# Accepted=True, and the conformance suite silently polls until timeout. Hit the
# exact endpoint the controller uses and fail fast instead.
# NB: do NOT use /user/tokens/verify — a minimum-privilege account token
# (Account -> Cloudflare Tunnel -> Edit) lacks user-level token introspection, so
# verify returns success:false / code 1000 for a working token. The
# configurations endpoint is the authoritative signal.
info "Validating Cloudflare API token against tunnel ${V2_TUNNEL_ID}..."
token_http_code="$(curl --silent --output /dev/null --write-out '%{http_code}' \
  --retry 3 --retry-delay 1 --max-time 15 \
  --header "Authorization: Bearer ${CF_API_TOKEN}" \
  "https://api.cloudflare.com/client/v4/accounts/${CF_ACCOUNT_ID}/cfd_tunnel/${V2_TUNNEL_ID}/configurations" || true)"
if [[ "${token_http_code}" != "200" ]]; then
  die "CF_API_TOKEN cannot read tunnel ${V2_TUNNEL_ID} (HTTP ${token_http_code:-no-response}). Refresh the token (Cloudflare dashboard -> account -> Cloudflare Tunnel -> Edit) before re-running."
fi

# --- CI-images mode: fail fast if the PR chart is gone before building a cluster ---
if [[ -n "${CI_PR_NUMBER}" ]]; then
  CI_CHART_VERSION="0.0.0-pr.${CI_PR_NUMBER}-1d"
  info "Checking ttl.sh chart for PR #${CI_PR_NUMBER} (${CI_CHART_VERSION})..."
  helm show chart "oci://ttl.sh/cloudflare-tunnel-gateway-controller" \
    --version "${CI_CHART_VERSION}" >/dev/null 2>&1 \
    || die "Chart ${CI_CHART_VERSION} not found on ttl.sh. ttl.sh artifacts expire after 24h (the '1d' tag) — re-run PR #${CI_PR_NUMBER}'s CI to republish."
fi

# --- Step 1: Ensure colima is running ---
info "Checking colima..."
if ! colima status >/dev/null 2>&1; then
  info "Starting colima..."
  colima start
fi

# --- Step 2: Delete old v2-test-* clusters ---
for old_cluster in $(kind get clusters 2>/dev/null | grep "^v2-test" || true); do
  info "Deleting old kind cluster '${old_cluster}'..."
  kind delete cluster --name "${old_cluster}"
done

# --- Step 3: Create fresh kind cluster (IPv4 only) ---
info "Creating kind cluster '${CLUSTER_NAME}'..."
cat <<KINDEOF | kind create cluster --name "${CLUSTER_NAME}" --wait 60s --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  ipFamily: ipv4
KINDEOF

# Verify connectivity
kubectl --context "${KUBE_CONTEXT}" cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || die "Cannot connect to cluster '${KUBE_CONTEXT}'"

# --- Step 4: Install Gateway API CRDs (experimental channel) ---
info "Installing Gateway API CRDs (${GATEWAY_API_VERSION}, experimental)..."
kubectl --context "${KUBE_CONTEXT}" apply \
  --server-side --force-conflicts \
  --filename "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml"

# --- Step 5: Build images ---
if [[ -n "${CI_PR_NUMBER}" ]]; then
  info "Using CI images from PR #${CI_PR_NUMBER} (ttl.sh) — skipping local build + kind load"
elif [[ "${SKIP_BUILD}" == "false" ]]; then
  info "Building controller image..."
  docker build --tag "${CONTROLLER_IMAGE}" --file Containerfile .

  info "Building proxy image..."
  docker build --tag "${PROXY_IMAGE}" --file Containerfile.proxy .
else
  info "Skipping image build (--skip-build)"
fi

# --- Step 6: Load images into kind ---
# In --use-ci-images mode there are no local images; kind nodes pull from
# ttl.sh at pod start (the PR chart ships pullPolicy=Always).
if [[ -z "${CI_PR_NUMBER}" ]]; then
  info "Loading images into kind cluster..."
  kind load docker-image "${CONTROLLER_IMAGE}" --name "${CLUSTER_NAME}"
  kind load docker-image "${PROXY_IMAGE}" --name "${CLUSTER_NAME}"
fi

# --- Step 7: Create namespace and secrets ---
info "Creating namespace '${NAMESPACE}'..."
kubectl --context "${KUBE_CONTEXT}" create namespace "${NAMESPACE}" --dry-run=client --output yaml \
  | kubectl --context "${KUBE_CONTEXT}" apply --filename -

info "Creating test namespace '${TEST_NAMESPACE}'..."
kubectl --context "${KUBE_CONTEXT}" create namespace "${TEST_NAMESPACE}" --dry-run=client --output yaml \
  | kubectl --context "${KUBE_CONTEXT}" apply --filename -

info "Creating Cloudflare credentials secret..."
kubectl --context "${KUBE_CONTEXT}" create secret generic cloudflare-credentials \
  --namespace "${NAMESPACE}" \
  --from-literal=api-token="${CF_API_TOKEN}" \
  --from-literal=account-id="${CF_ACCOUNT_ID}" \
  --dry-run=client --output yaml \
  | kubectl --context "${KUBE_CONTEXT}" apply --filename -

info "Creating tunnel token secret..."
kubectl --context "${KUBE_CONTEXT}" create secret generic cloudflare-tunnel-token \
  --namespace "${NAMESPACE}" \
  --from-literal=tunnel-token="${V2_TUNNEL_TOKEN}" \
  --dry-run=client --output yaml \
  | kubectl --context "${KUBE_CONTEXT}" apply --filename -

# --- Step 8: Deploy via helm ---
# Default: install the local chart and point it at the locally-built images.
# --use-ci-images: install the PR's published ttl.sh chart, which already
# carries the ttl.sh image refs + pullPolicy=Always baked into its values.yaml,
# so no --set image.* overrides are needed (or wanted) in that mode.
HELM_CHART_REF="${REPO_ROOT}/charts/cloudflare-tunnel-gateway-controller"
HELM_VERSION_ARGS=()
HELM_IMAGE_ARGS=(
  --set image.repository="${CONTROLLER_IMAGE%%:*}"
  --set image.tag="${CONTROLLER_IMAGE##*:}"
  --set image.pullPolicy=Never
  --set proxy.image.repository="${PROXY_IMAGE%%:*}"
  --set proxy.image.tag="${PROXY_IMAGE##*:}"
  --set proxy.image.pullPolicy=Never
)
if [[ -n "${CI_PR_NUMBER}" ]]; then
  HELM_CHART_REF="oci://ttl.sh/cloudflare-tunnel-gateway-controller"
  HELM_VERSION_ARGS=(--version "${CI_CHART_VERSION}")
  HELM_IMAGE_ARGS=()
fi

# proxy.tunnel.protocol=http2 is mandatory in both modes: the chart defaults to
# "auto" (QUIC-first) and kind/Colima blocks QUIC egress to the CF edge (530/1033).
# The "${arr[@]+...}" idiom expands an empty array to nothing without tripping
# `set -u` on bash < 4.4 (stock macOS ships 3.2).
info "Deploying controller via helm..."
helm upgrade --install "${RELEASE_NAME}" \
  "${HELM_CHART_REF}" \
  "${HELM_VERSION_ARGS[@]+"${HELM_VERSION_ARGS[@]}"}" \
  --kube-context "${KUBE_CONTEXT}" \
  --namespace "${NAMESPACE}" \
  --set gatewayClassConfig.create=true \
  --set gatewayClassConfig.tunnelID="${V2_TUNNEL_ID}" \
  --set gatewayClassConfig.cloudflareCredentialsSecretRef.name=cloudflare-credentials \
  "${HELM_IMAGE_ARGS[@]+"${HELM_IMAGE_ARGS[@]}"}" \
  --set proxy.tunnelTokenSecretRef.name=cloudflare-tunnel-token \
  --set proxy.tunnel.protocol=http2 \
  --set controller.logLevel=debug \
  --wait --timeout 120s

# --- Step 9: Wait for readiness ---
info "Waiting for controller deployment..."
kubectl --context "${KUBE_CONTEXT}" rollout status deployment \
  --namespace "${NAMESPACE}" \
  "${RELEASE_NAME}-cloudflare-tunnel-gateway-controller" \
  --timeout=120s

info "Waiting for proxy deployment..."
kubectl --context "${KUBE_CONTEXT}" rollout status deployment \
  --namespace "${NAMESPACE}" \
  "${RELEASE_NAME}-cloudflare-tunnel-gateway-controller-proxy" \
  --timeout=120s

info "All deployments ready!"

# --- Step 10: Show status ---
echo ""
info "Cluster: ${CLUSTER_NAME} (context: ${KUBE_CONTEXT})"
kubectl --context "${KUBE_CONTEXT}" get pods --namespace "${NAMESPACE}"
echo ""
kubectl --context "${KUBE_CONTEXT}" get gatewayclass 2>/dev/null || true
echo ""

# --- Step 11: Run tests (optional) ---
if [[ "${RUN_TESTS}" == "true" ]]; then
  info "Running conformance tests against ${V2_TUNNEL_HOSTNAME}..."
  CONFORMANCE_KUBE_CONTEXT="${KUBE_CONTEXT}" \
  CONFORMANCE_TUNNEL_HOSTNAME="${V2_TUNNEL_HOSTNAME}" \
    go test -v -race -tags conformance -count=1 -timeout=60m -parallel 10 ./test/conformance/...
else
  echo ""
  info "Setup complete! To run conformance tests:"
  echo "  CONFORMANCE_KUBE_CONTEXT=${KUBE_CONTEXT} CONFORMANCE_TUNNEL_HOSTNAME=${V2_TUNNEL_HOSTNAME} go test -v -race -tags conformance -count=1 -timeout=30m ./test/conformance/..."
  echo ""
  info "To run E2E tests:"
  echo "  CONFORMANCE_KUBE_CONTEXT=${KUBE_CONTEXT} E2E_TUNNEL_HOSTNAME=${V2_TUNNEL_HOSTNAME} go test -v -race -tags e2e -count=1 -timeout=15m ./test/e2e/..."
  echo ""
  info "To tear down:"
  echo "  kind delete cluster --name ${CLUSTER_NAME}"
fi
