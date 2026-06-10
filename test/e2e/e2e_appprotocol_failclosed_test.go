//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestBackendAppProtocolTLSWithoutPolicyFailsClosed pins the fail-closed
// contract end to end through the real tunnel: a Service port declaring a TLS
// appProtocol (`https`) with NO BackendTLSPolicy attached means the operator
// asked for TLS but the proxy has no CA to verify the backend -- the spec
// says the implementation must not silently dial cleartext, so the backend's
// traffic fraction answers 502 instead of reaching the (cleartext) pod.
func TestBackendAppProtocolTLSWithoutPolicyFailsClosed(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	// A Service selecting the plain-HTTP echo-v1 pods but declaring the port
	// as appProtocol https. The pods serve cleartext; only the hint lies.
	appProto := "https"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-v1-tls-hint", Namespace: cfg.TestNamespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "echo-v1"},
			Ports: []corev1.ServicePort{{
				Name:        "https-hint",
				Port:        80,
				TargetPort:  intstr.FromInt32(3000),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: &appProto,
			}},
		},
	}

	applyObject(ctx, t, k8sClient, svc)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), svc)
	})

	route := buildHTTPRoute("fail-closed", cfg, []gatewayv1.HTTPRouteRule{{
		Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathExact("/fail-closed")}},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1-tls-hint", 80, nil)},
	}})
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), route)
	})

	// Status first: ResolvedRefs=False/UnsupportedProtocol proves the
	// controller has SEEN the TLS hint and pushed the fail-closed config
	// (the config push happens before the status write), which closes the
	// stale-cache window where an early reconcile could briefly serve
	// cleartext before the Service lands in the informer cache. Only after
	// that is a 200 a genuine spec violation.
	waitForRouteResolvedRefsFalse(t, k8sClient, route, string(gatewayv1.RouteReasonUnsupportedProtocol))

	// Data plane: the proxy must answer 502 (Bad Gateway) for the route --
	// never 200 (which would now mean a silent cleartext dial).
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			_, resp, reqErr := makeRequest(pollCtx, t, httpClient, cfg.TunnelHostname, http.MethodGet, "/fail-closed", nil)
			if reqErr != nil {
				return false, nil //nolint:nilerr // transient edge/tunnel errors are expected while polling; retry until timeout
			}

			if resp.StatusCode == http.StatusOK {
				return false, errStrictFailClosed
			}

			return resp.StatusCode == http.StatusBadGateway, nil
		},
	)
	require.NoError(t, err, "a TLS appProtocol without a BackendTLSPolicy must answer 502, never reach the backend in cleartext")
}

// errStrictFailClosed aborts the poll immediately: a 200 means the proxy
// dialed the backend in cleartext despite the TLS hint -- the exact
// behaviour the spec forbids, and waiting longer cannot fix it.
var errStrictFailClosed = failClosedError("appProtocol https without policy answered 200: silent cleartext dial")

type failClosedError string

func (e failClosedError) Error() string { return string(e) }

// waitForRouteResolvedRefsFalse polls the route until one of its parents
// carries ResolvedRefs=False with the given reason.
func waitForRouteResolvedRefsFalse(t *testing.T, k8sClient client.Client, route *gatewayv1.HTTPRoute, reason string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			current := &gatewayv1.HTTPRoute{}
			getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, current)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors are expected while polling; retry until timeout
			}

			for _, parent := range current.Status.Parents {
				for _, cond := range parent.Conditions {
					if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) &&
						cond.Status == metav1.ConditionFalse && cond.Reason == reason {
						return true, nil
					}
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "route never reported ResolvedRefs=False/%s", reason)
}
