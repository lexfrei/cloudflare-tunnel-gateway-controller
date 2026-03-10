#!/usr/bin/env bash
set -euo pipefail

# E2E test script for v2 in-process proxy delegation
# Prerequisites: kind cluster "v2-test", images loaded, Gateway API CRDs installed

CONTEXT="kind-v2-test"
NAMESPACE="cloudflare-tunnel-system"
TEST_NS="e2e-test"
HOSTNAME="v2-test.lex.la"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $1"; }
fail() { echo -e "${RED}[FAIL]${NC} $1"; FAILURES=$((FAILURES + 1)); }
info() { echo -e "${YELLOW}[INFO]${NC} $1"; }

FAILURES=0
TESTS=0

# Wait for a deployment to be ready
wait_ready() {
    local ns=$1 name=$2 timeout=${3:-120}
    info "Waiting for deployment $ns/$name (timeout: ${timeout}s)..."
    kubectl --context "$CONTEXT" --namespace "$ns" rollout status deployment/"$name" --timeout="${timeout}s"
}

# Run a single test
run_test() {
    local name=$1
    local url=$2
    local expected=$3
    local extra_args=${4:-}
    TESTS=$((TESTS + 1))

    local response
    # shellcheck disable=SC2086
    response=$(curl --silent --max-time 10 $extra_args "$url" 2>&1) || true

    if echo "$response" | grep -q "$expected"; then
        pass "$name"
    else
        fail "$name (expected '$expected', got: $(echo "$response" | head -1))"
    fi
}

# Run weighted test (multiple requests, check distribution)
run_weighted_test() {
    local name=$1
    local url=$2
    local count=${3:-20}
    TESTS=$((TESTS + 1))

    local count_a=0 count_b=0
    info "Running $count requests for weighted test..."

    for _ in $(seq 1 "$count"); do
        response=$(curl --silent --max-time 10 "$url" 2>&1) || true
        if echo "$response" | grep -q "backend-a"; then
            count_a=$((count_a + 1))
        elif echo "$response" | grep -q "backend-b"; then
            count_b=$((count_b + 1))
        fi
    done

    info "Weight distribution: backend-a=$count_a, backend-b=$count_b (total=$count)"

    # With 80/20 split over 20 requests, backend-a should get most traffic
    # Allow wide margin: backend-a >= 10 (50%) is enough to confirm weighting works
    if [ "$count_a" -ge 10 ] && [ "$count_b" -ge 1 ]; then
        pass "$name (a=$count_a, b=$count_b)"
    else
        fail "$name (unexpected distribution: a=$count_a, b=$count_b)"
    fi
}

echo "========================================="
echo "  E2E Test Suite: v2 In-Process Proxy"
echo "========================================="
echo ""

# Step 1: Create secrets
info "Creating secrets..."
kubectl --context "$CONTEXT" create namespace "$NAMESPACE" --dry-run=client --output yaml | kubectl --context "$CONTEXT" apply --filename -

# Source .env for tokens
# shellcheck disable=SC1091
source "$SCRIPT_DIR/../.env"

kubectl --context "$CONTEXT" --namespace "$NAMESPACE" create secret generic cloudflare-credentials \
    --from-literal=api-token="$CF_API_TOKEN" \
    --dry-run=client --output yaml | kubectl --context "$CONTEXT" apply --filename -

kubectl --context "$CONTEXT" --namespace "$NAMESPACE" create secret generic cloudflare-tunnel-token \
    --from-literal=tunnel-token="$V2_TUNNEL_TOKEN" \
    --dry-run=client --output yaml | kubectl --context "$CONTEXT" apply --filename -

# Step 2: Install chart
info "Installing Helm chart..."
helm upgrade --install cftunnel \
    "$SCRIPT_DIR/../charts/cloudflare-tunnel-gateway-controller" \
    --namespace "$NAMESPACE" \
    --create-namespace \
    --values "$SCRIPT_DIR/values-e2e.yaml" \
    --kube-context "$CONTEXT" \
    --timeout 120s

# Wait for controller to be ready (it has no config dependency)
wait_ready "$NAMESPACE" cftunnel-cloudflare-tunnel-gateway-controller 120

# Wait for proxy to be running (not ready — readiness requires config push)
info "Waiting for proxy pod to start..."
for i in $(seq 1 30); do
    phase=$(kubectl --context "$CONTEXT" --namespace "$NAMESPACE" get pods --selector app.kubernetes.io/component=proxy --output jsonpath='{.items[0].status.phase}' 2>/dev/null)
    if [ "$phase" = "Running" ]; then
        info "Proxy pod is running"
        break
    fi
    if [ "$i" -eq 30 ]; then
        fail "Proxy pod did not reach Running state"
        kubectl --context "$CONTEXT" --namespace "$NAMESPACE" describe pods --selector app.kubernetes.io/component=proxy
        exit 1
    fi
    sleep 5
done

# Step 3: Deploy backends
info "Deploying backend services..."
kubectl --context "$CONTEXT" apply --filename "$SCRIPT_DIR/backend.yaml"
wait_ready "$TEST_NS" echo-a 60
wait_ready "$TEST_NS" echo-b 60

# Step 4: Create Gateway
info "Creating Gateway..."
kubectl --context "$CONTEXT" apply --filename "$SCRIPT_DIR/gateway.yaml"
sleep 5

# Step 5: Create HTTPRoutes
info "Creating HTTPRoutes..."
kubectl --context "$CONTEXT" apply --filename "$SCRIPT_DIR/httproutes.yaml"

# Step 6: Wait for controller to sync
info "Waiting for controller to process routes..."
sleep 15

# Step 7: Check pod status
info "Pod status:"
kubectl --context "$CONTEXT" --namespace "$NAMESPACE" get pods
kubectl --context "$CONTEXT" --namespace "$TEST_NS" get pods
echo ""

# Step 8: Check controller logs for proxy sync
info "Controller logs (last 20 lines):"
kubectl --context "$CONTEXT" --namespace "$NAMESPACE" logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller --tail 20 || true
echo ""

info "Proxy logs (last 20 lines):"
kubectl --context "$CONTEXT" --namespace "$NAMESPACE" logs --selector app.kubernetes.io/component=proxy --tail 20 || true
echo ""

# Step 9: Wait for tunnel to connect
info "Waiting for tunnel to establish connection (up to 60s)..."
for i in $(seq 1 12); do
    if curl --silent --max-time 5 "https://$HOSTNAME/" | grep -q "backend"; then
        info "Tunnel is connected!"
        break
    fi
    if [ "$i" -eq 12 ]; then
        fail "Tunnel did not connect within 60s"
        info "Proxy logs:"
        kubectl --context "$CONTEXT" --namespace "$NAMESPACE" logs --selector app.kubernetes.io/component=proxy --tail 50 || true
        echo ""
        echo "========================================="
        echo "  Results: $FAILURES failures"
        echo "========================================="
        exit 1
    fi
    sleep 5
done

echo ""
echo "========================================="
echo "  Running HTTPRoute Tests"
echo "========================================="
echo ""

# Test 1: Basic PathPrefix
run_test "PathPrefix /echo-a" "https://$HOSTNAME/echo-a" "backend-a"

# Test 2: Exact path
run_test "Exact /exact" "https://$HOSTNAME/exact" "backend-b"

# Test 3: Exact path miss (should hit catch-all)
run_test "Exact /exact/sub (miss → catch-all)" "https://$HOSTNAME/exact/sub" "backend-a"

# Test 4: Weighted traffic split
run_weighted_test "Weighted /weighted (80/20)" "https://$HOSTNAME/weighted"

# Test 5: Header-based routing
run_test "Header match X-Route-To:backend-b" "https://$HOSTNAME/header-test" "backend-b" "--header X-Route-To:backend-b"

# Test 6: Header match (no header → default)
run_test "Header match (no header → default)" "https://$HOSTNAME/header-test" "backend-a"

# Test 7: Method match GET
run_test "Method GET /method-test" "https://$HOSTNAME/method-test" "backend-a"

# Test 8: Method match POST
run_test "Method POST /method-test" "https://$HOSTNAME/method-test" "backend-b" "--request POST"

# Test 9: Catch-all
run_test "Catch-all /random-path" "https://$HOSTNAME/random-path" "backend-a"

echo ""
echo "========================================="
echo "  Results: $TESTS tests, $FAILURES failures"
echo "========================================="

if [ "$FAILURES" -gt 0 ]; then
    exit 1
fi
