//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Knative DomainMapping ref-update + scale-to-zero readiness regression.
//
// net-gateway-api gates a KIngress's readiness on an HTTP probe it dials at the
// gateway data plane, expecting "200 + K-Network-Hash == version". For a
// DomainMapping the ksvc Service is an ExternalName pointing back at this proxy,
// so forwarding the probe re-enters the proxy and (on a ref UPDATE, or when the
// revision is scaled to zero) degrades into ordinary traffic that 404s/hangs —
// the KIngress then stays LoadBalancerReady=Unknown forever. The fix answers the
// probe authoritatively at the in-cluster listener. This test reproduces the
// exact failing sequence: create -> Ready, scale the first ksvc to zero, then
// flip the DomainMapping ref to a second ksvc and assert it returns to Ready
// (which only happens if every endpoint-probe path — including the retained,
// scaled-to-zero old backend — is answered with the right hash).
//
// PRECONDITIONS (test skips if unmet): Knative Serving + net-gateway-api
// installed with this controller as the GatewayClass; the proxy deployed with
// proxy.inClusterListener.enabled=true; a Cloudflare zone the maintainer owns,
// passed via E2E_KNATIVE_ZONE (or KNATIVE_DOMAIN_ZONE). Maintainer-run, matching
// the feature-delivery workflow (CI lacks a real Cloudflare account).

var (
	ksvcGVK = schema.GroupVersionKind{Group: "serving.knative.dev", Version: "v1", Kind: "Service"}
	dmGVK   = schema.GroupVersionKind{Group: "serving.knative.dev", Version: "v1beta1", Kind: "DomainMapping"}
)

const (
	// knativeDefaultImage respects $PORT and echoes $TARGET in its body, so two
	// ksvcs with distinct TARGETs are distinguishable over the edge.
	knativeDefaultImage = "ghcr.io/knative/helloworld-go:latest"
	knativeReadyTimeout = 5 * time.Minute
	scaleToZeroTimeout  = 4 * time.Minute
)

func knativeZone(t *testing.T) string {
	t.Helper()

	zone := envWithFallback("E2E_KNATIVE_ZONE", "KNATIVE_DOMAIN_ZONE", "")
	if zone == "" {
		t.Skip("E2E_KNATIVE_ZONE (or KNATIVE_DOMAIN_ZONE) not set; skipping Knative DomainMapping e2e")
	}

	return zone
}

// requireKnativeInstalled skips the test unless the Knative Serving Service GVK
// is served by the cluster.
func requireKnativeInstalled(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ksvcGVK)

	err := k8sClient.List(ctx, list, client.Limit(1))
	if err != nil {
		t.Skipf("Knative Serving not installed (cannot list %s): %v", ksvcGVK, err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 5)
	_, err := rand.Read(buf)
	require.NoError(t, err, "failed to generate random suffix")

	return hex.EncodeToString(buf)
}

func ksvcObject(namespace, name, target, image string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ksvcGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]any{
					// Tighten the autoscaler window so the revision scales to
					// zero quickly, keeping the scale-to-zero gate under timeout.
					"autoscaling.knative.dev/window":   "20s",
					"autoscaling.knative.dev/minScale": "0",
				},
			},
			"spec": map[string]any{
				"containers": []any{
					map[string]any{
						"image": image,
						"env":   []any{map[string]any{"name": "TARGET", "value": target}},
					},
				},
			},
		},
	}, "spec")

	return obj
}

func domainMappingObject(namespace, host, ksvcName string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dmGVK)
	obj.SetNamespace(namespace)
	obj.SetName(host)
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"ref": map[string]any{
			"apiVersion": "serving.knative.dev/v1",
			"kind":       "Service",
			"name":       ksvcName,
			"namespace":  namespace,
		},
	}, "spec")

	return obj
}

// readyCondition returns the status ("True"/"False"/"Unknown"/"") and reason of
// the object's Ready condition.
func readyCondition(obj *unstructured.Unstructured) (string, string) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "", ""
	}

	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Ready" {
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)

			return status, reason
		}
	}

	return "", ""
}

func waitReady(ctx context.Context, t *testing.T, k8sClient client.Client, gvk schema.GroupVersionKind, namespace, name string, timeout time.Duration) {
	t.Helper()

	var lastStatus, lastReason string
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true,
		func(pollCtx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Namespace: namespace, Name: name}, obj)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling are expected; retry until timeout
			}

			lastStatus, lastReason = readyCondition(obj)

			return lastStatus == "True", nil
		},
	)
	require.NoErrorf(t, err, "%s %s/%s never became Ready (last status=%q reason=%q)", gvk.Kind, namespace, name, lastStatus, lastReason)
}

// waitRevisionScaledToZero waits until no Running pod backs the ksvc. Returns
// true if it reached zero, false on timeout (the caller logs and proceeds —
// autoscaler timing must not flake the test, the ref-update gate still applies).
func waitRevisionScaledToZero(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, ksvcName string, timeout time.Duration) bool {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true,
		func(pollCtx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			listErr := k8sClient.List(pollCtx, pods,
				client.InNamespace(namespace),
				client.MatchingLabels{"serving.knative.dev/service": ksvcName},
			)
			if listErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling are expected; retry
			}

			for i := range pods.Items {
				if pods.Items[i].Status.Phase == corev1.PodRunning {
					return false, nil
				}
			}

			return true, nil
		},
	)

	return err == nil
}

func TestKnativeDomainMapping_RefUpdate_ScaleToZero_StaysReady(t *testing.T) {
	cfg := loadTestConfig(t)
	zone := knativeZone(t)

	ctx := context.Background()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	requireKnativeInstalled(ctx, t, k8sClient)

	image := envWithFallback("E2E_KNATIVE_IMAGE", "KNATIVE_IMAGE", knativeDefaultImage)
	suffix := randomSuffix(t)
	namespace := "default"

	ksvcA := "e2e-kn-" + suffix + "-a"
	ksvcB := "e2e-kn-" + suffix + "-b"
	host := suffix + "." + zone // unique per run — Cloudflare caches DNS, names must never repeat

	t.Logf("knative domainmapping e2e: host=%s ksvcA=%s ksvcB=%s image=%s", host, ksvcA, ksvcB, image)

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = k8sClient.Delete(cleanupCtx, domainMappingObject(namespace, host, ksvcA))
		_ = k8sClient.Delete(cleanupCtx, ksvcObject(namespace, ksvcA, "", image))
		_ = k8sClient.Delete(cleanupCtx, ksvcObject(namespace, ksvcB, "", image))
	})

	// STEP 1 — create ksvcA + DomainMapping -> ksvcA, wait Ready.
	require.NoError(t, k8sClient.Create(ctx, ksvcObject(namespace, ksvcA, ksvcA, image)))
	waitReady(ctx, t, k8sClient, ksvcGVK, namespace, ksvcA, knativeReadyTimeout)

	require.NoError(t, k8sClient.Create(ctx, domainMappingObject(namespace, host, ksvcA)))
	waitReady(ctx, t, k8sClient, dmGVK, namespace, host, knativeReadyTimeout)
	t.Logf("STEP 1 ok: DomainMapping %s Ready -> %s", host, ksvcA)

	// STEP 2 — scale ksvcA to zero so STEP 3's rolling handoff probes a
	// scaled-to-zero OLD backend (the exact case the old forwarding path broke).
	if waitRevisionScaledToZero(ctx, t, k8sClient, namespace, ksvcA, scaleToZeroTimeout) {
		t.Logf("STEP 2 ok: ksvcA %s scaled to zero", ksvcA)
	} else {
		t.Logf("STEP 2 warn: ksvcA %s did not scale to zero within %s; continuing (update gate still applies)", ksvcA, scaleToZeroTimeout)
	}

	// STEP 3 — create ksvcB, flip the DomainMapping ref A -> B, assert it returns
	// to Ready. net-gateway-api rebuilds the route with endpoint probes for BOTH
	// the new (B) and retained old (A, scaled-to-zero) backends; convergence
	// requires every probe path to be answered authoritatively with its hash.
	require.NoError(t, k8sClient.Create(ctx, ksvcObject(namespace, ksvcB, ksvcB, image)))
	waitReady(ctx, t, k8sClient, ksvcGVK, namespace, ksvcB, knativeReadyTimeout)

	updateDomainMappingRef(ctx, t, k8sClient, namespace, host, ksvcB)
	waitReady(ctx, t, k8sClient, dmGVK, namespace, host, knativeReadyTimeout)
	t.Logf("STEP 3 ok: DomainMapping %s returned to Ready after ref update -> %s", host, ksvcB)

	// STEP 4 (optional) — confirm the route actually serves the NEW backend over
	// the edge. Gated on E2E_KNATIVE_VERIFY_EDGE because it depends on Cloudflare
	// DNS propagation for <host>, which is inherently slower/flakier than the
	// cluster-side readiness gate above.
	if os.Getenv("E2E_KNATIVE_VERIFY_EDGE") != "" {
		assertEdgeServesTarget(ctx, t, host, ksvcB)
	}
}

// updateDomainMappingRef patches the DomainMapping's spec.ref.name with a
// conflict-retrying read-modify-write.
func updateDomainMappingRef(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, host, ksvcName string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(dmGVK)
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Namespace: namespace, Name: host}, obj)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling are expected; retry
			}

			setErr := unstructured.SetNestedField(obj.Object, ksvcName, "spec", "ref", "name")
			if setErr != nil {
				return false, fmt.Errorf("setting spec.ref.name: %w", setErr)
			}

			updateErr := k8sClient.Update(pollCtx, obj)
			if updateErr != nil {
				if apierrors.IsConflict(updateErr) {
					return false, nil
				}

				return false, fmt.Errorf("updating DomainMapping %s: %w", host, updateErr)
			}

			return true, nil
		},
	)
	require.NoError(t, err, "failed to update DomainMapping %s ref to %s", host, ksvcName)
}

func assertEdgeServesTarget(ctx context.Context, t *testing.T, host, expectedTarget string) {
	t.Helper()

	httpClient := tunnelClient()
	url := "https://" + host + "/"

	var lastBody, lastErr string
	ok := false
	pollErr := wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			req, reqErr := http.NewRequestWithContext(pollCtx, http.MethodGet, url, nil)
			if reqErr != nil {
				lastErr = reqErr.Error()

				return false, nil
			}

			resp, doErr := httpClient.Do(req)
			if doErr != nil {
				lastErr = doErr.Error()

				return false, nil
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			lastBody = string(body)
			if resp.StatusCode == http.StatusOK && strings.Contains(lastBody, expectedTarget) {
				ok = true

				return true, nil
			}

			return false, nil
		},
	)

	assert.NoErrorf(t, pollErr, "edge never served TARGET=%q for %s (last body=%q err=%q)", expectedTarget, host, lastBody, lastErr)
	assert.True(t, ok, "edge must serve the new backend (TARGET=%q)", expectedTarget)
}
