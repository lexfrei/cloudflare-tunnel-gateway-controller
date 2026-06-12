//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestProxyMetricsEndpoint pins the data-plane metrics surface (#473) through
// a real cluster: after live traffic has flowed through the tunnel, the proxy
// pods' /metrics on the config-api port must expose the cftunnel_proxy_*
// instruments (with a non-zero 2xx counter) AND the embedded cloudflared
// connector metrics merged into the same exposition.
func TestProxyMetricsEndpoint(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	clientset := newClientset(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	route := buildHTTPRoute("metrics-e2e-route", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/metrics-e2e")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), route)
	})

	// Drive at least one real request through the data plane so the request
	// counters have a sample to show.
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/metrics-e2e", "echo-v1-", 90*time.Second)

	serviceName := proxyConfigServiceName(t, k8sClient, cfg.Namespace)

	ctx := context.Background()

	var exposition string

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			raw, getErr := clientset.CoreV1().Services(cfg.Namespace).
				ProxyGet("http", serviceName, "config-api", "/metrics", nil).
				DoRaw(pollCtx)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient proxy/API errors while polling
			}

			exposition = string(raw)

			return strings.Contains(exposition, "cftunnel_proxy_requests_total"), nil
		},
	)
	require.NoError(t, err, "proxy /metrics never exposed the data-plane instruments")

	assert.Contains(t, exposition, `cftunnel_proxy_requests_total`, "request counters must be exposed")
	assert.Contains(t, exposition, `status_class="2xx"`, "the live request must have been counted")
	assert.Contains(t, exposition, "cftunnel_proxy_requests_in_flight", "the HPA saturation gauge must be exposed")
	assert.Contains(t, exposition, "cloudflared_tunnel_", "embedded cloudflared connector metrics must ride the same endpoint")
}

// proxyConfigServiceName finds the shared proxy Service (component=proxy)
// that exposes the config-api port the ServiceMonitor scrapes.
func proxyConfigServiceName(t *testing.T, k8sClient client.Client, namespace string) string {
	t.Helper()

	var services corev1.ServiceList
	require.NoError(t, k8sClient.List(context.Background(), &services,
		client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "proxy"},
	))

	for i := range services.Items {
		for _, port := range services.Items[i].Spec.Ports {
			if port.Name == "config-api" {
				return services.Items[i].Name
			}
		}
	}

	t.Fatalf("no proxy Service with a config-api port found in %s", namespace)

	return ""
}
