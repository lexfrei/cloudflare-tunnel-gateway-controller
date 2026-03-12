#!/usr/bin/env bash
# conformance-setup.sh — Reproducible setup for Gateway API conformance tests.
#
# This script:
#   1. Ensures colima is running
#   2. Deletes old v2-test-* kind clusters
#   3. Creates a fresh kind cluster with random suffix
#   4. Installs Gateway API CRDs (experimental channel)
#   5. Builds controller + proxy images
#   6. Loads images into kind
#   7. Creates secrets from .env
#   8. Deploys controller via helm
#   9. Waits for readiness
#  10. Optionally runs conformance tests
#
# Usage:
#   ./hack/conformance-setup.sh              # full setup (fresh cluster)
#   ./hack/conformance-setup.sh --test       # setup + run tests
#   ./hack/conformance-setup.sh --skip-build # skip image build (reuse existing)
#
# Prerequisites:
#   - .env file in repo root with: CF_API_TOKEN, CF_ACCOUNT_ID, V2_TUNNEL_ID, V2_TUNNEL_TOKEN
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
GATEWAY_API_VERSION="v1.5.0"

# --- Flags ---
RUN_TESTS=false
SKIP_BUILD=false

for arg in "$@"; do
  case "${arg}" in
    --test) RUN_TESTS=true ;;
    --skip-build) SKIP_BUILD=true ;;
    *) echo "Unknown flag: ${arg}"; exit 1 ;;
  esac
done

# --- Helpers ---
info()  { echo "==> $*"; }
warn()  { echo "WARN: $*" >&2; }
die()   { echo "ERROR: $*" >&2; exit 1; }

check_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not installed"
}

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

for var in CF_API_TOKEN CF_ACCOUNT_ID V2_TUNNEL_ID V2_TUNNEL_TOKEN; do
  if [[ -z "${!var:-}" ]]; then
    die "Required variable ${var} is not set in .env"
  fi
done

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
if [[ "${SKIP_BUILD}" == "false" ]]; then
  info "Building controller image..."
  docker build --tag "${CONTROLLER_IMAGE}" --file Containerfile .

  info "Building proxy image..."
  docker build --tag "${PROXY_IMAGE}" --file Containerfile.proxy .
else
  info "Skipping image build (--skip-build)"
fi

# --- Step 6: Load images into kind ---
info "Loading images into kind cluster..."
kind load docker-image "${CONTROLLER_IMAGE}" --name "${CLUSTER_NAME}"
kind load docker-image "${PROXY_IMAGE}" --name "${CLUSTER_NAME}"

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
info "Deploying controller via helm..."
helm upgrade --install "${RELEASE_NAME}" \
  "${REPO_ROOT}/charts/cloudflare-tunnel-gateway-controller" \
  --kube-context "${KUBE_CONTEXT}" \
  --namespace "${NAMESPACE}" \
  --set gatewayClassConfig.create=true \
  --set gatewayClassConfig.tunnelID="${V2_TUNNEL_ID}" \
  --set gatewayClassConfig.cloudflareCredentialsSecretRef.name=cloudflare-credentials \
  --set gatewayClassConfig.tunnelTokenSecretRef.name=cloudflare-tunnel-token \
  --set gatewayClassConfig.cloudflared.enabled=false \
  --set image.repository="${CONTROLLER_IMAGE%%:*}" \
  --set image.tag="${CONTROLLER_IMAGE##*:}" \
  --set image.pullPolicy=Never \
  --set proxy.enabled=true \
  --set proxy.image.repository="${PROXY_IMAGE%%:*}" \
  --set proxy.image.tag="${PROXY_IMAGE##*:}" \
  --set proxy.image.pullPolicy=Never \
  --set proxy.tunnelTokenSecretRef.name=cloudflare-tunnel-token \
  --set controller.logLevel=debug \
  --wait --timeout 120s

# --- Step 9: Wait for readiness ---
info "Waiting for controller deployment..."
kubectl --context "${KUBE_CONTEXT}" rollout status deployment \
  --namespace "${NAMESPACE}" \
  "${RELEASE_NAME}-cloudflare-tunnel-gateway-controller" \
  --timeout=120s

if kubectl --context "${KUBE_CONTEXT}" get deployment \
  --namespace "${NAMESPACE}" \
  "${RELEASE_NAME}-cloudflare-tunnel-gateway-controller-proxy" >/dev/null 2>&1; then
  info "Waiting for proxy deployment..."
  kubectl --context "${KUBE_CONTEXT}" rollout status deployment \
    --namespace "${NAMESPACE}" \
    "${RELEASE_NAME}-cloudflare-tunnel-gateway-controller-proxy" \
    --timeout=120s
fi

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
  info "Running conformance tests..."
  CONFORMANCE_KUBE_CONTEXT="${KUBE_CONTEXT}" \
    go test -v -race -tags conformance -count=1 -timeout=60m -parallel 10 ./test/conformance/...
else
  echo ""
  info "Setup complete! To run conformance tests:"
  echo "  CONFORMANCE_KUBE_CONTEXT=${KUBE_CONTEXT} go test -v -race -tags conformance -count=1 -timeout=30m ./test/conformance/..."
  echo ""
  info "To run E2E tests:"
  echo "  CONFORMANCE_KUBE_CONTEXT=${KUBE_CONTEXT} go test -v -race -tags e2e -count=1 -timeout=15m ./test/e2e/..."
  echo ""
  info "To tear down:"
  echo "  kind delete cluster --name ${CLUSTER_NAME}"
fi
