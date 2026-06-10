//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestExternalBackendEndToEnd pins the ExternalBackend CRD path through the
// real tunnel: an HTTPRoute backendRef of kind ExternalBackend resolves to a
// direct-dial URL from the CR spec (the proxy dials it without any Service
// resolution; the backendRef port is ignored in favour of spec.port). The
// "external" origin here is the echo Service's cluster DNS name -- from the
// proxy's perspective it is just a URL to dial, which is exactly the
// ExternalBackend contract.
func TestExternalBackendEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	external := &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-echo", Namespace: cfg.TestNamespace},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendSchemeHTTP,
			Host:   "echo-v2." + cfg.TestNamespace + ".svc.cluster.local",
			Port:   80,
			// Base path: the proxy must prepend it to every dialed request,
			// so the backend sees /ext-base<request-path>.
			Path: "/ext-base",
		},
	}

	createExternalBackend(t, k8sClient, external)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), external)
	})

	group := gatewayv1.Group("cf.k8s.lex.la")
	kind := gatewayv1.Kind("ExternalBackend")
	port := gatewayv1.PortNumber(80)

	route := buildHTTPRoute("ext-backend", cfg, []gatewayv1.HTTPRouteRule{{
		Matches: []gatewayv1.HTTPRouteMatch{{Path: pathExact("/ext-backend")}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: &group,
					Kind:  &kind,
					Name:  gatewayv1.ObjectName(external.Name),
					Port:  &port, // ignored in favour of spec.port per the CRD contract
				},
			},
		}},
	}})
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), route)
	})

	// waitForBackend polls until the route serves; the echo pod reports the
	// path it RECEIVED, which must carry the ExternalBackend base path.
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			echo, resp, reqErr := makeRequest(pollCtx, t, httpClient, cfg.TunnelHostname, http.MethodGet, "/ext-backend", nil)
			if reqErr != nil || resp.StatusCode != http.StatusOK {
				return false, nil //nolint:nilerr // transient edge/tunnel errors are expected while polling; retry until timeout
			}

			return strings.HasPrefix(echo.Pod, "echo-v2-"), nil
		},
	)
	require.NoError(t, err, "the ExternalBackend route never started serving")

	echo, resp, err := makeRequest(context.Background(), t, httpClient, cfg.TunnelHostname, http.MethodGet, "/ext-backend", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "/ext-base/ext-backend", echo.Path,
		"the backend must receive the request path prefixed with the ExternalBackend base path")
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"),
		"the ExternalBackend URL must be dialed directly and reach the echo pod, got pod %q", echo.Pod)
}

// createExternalBackend creates (or replaces) an ExternalBackend CR.
func createExternalBackend(t *testing.T, k8sClient client.Client, backend *v1alpha1.ExternalBackend) {
	t.Helper()
	applyObject(context.Background(), t, k8sClient, backend)
	t.Logf("created ExternalBackend %s/%s", backend.Namespace, backend.Name)
}
