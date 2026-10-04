//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

const (
	// secondTunnelHostnameEnvVar names the edge hostname of a second test
	// tunnel; its connector token is the Secret secondTunnelTokenSecret.
	secondTunnelHostnameEnvVar = "E2E_TUNNEL_2_HOSTNAME"
	secondTunnelTokenSecret    = "cloudflare-tunnel-2-token"

	planeSharedGateway    = "mtls-tunnel-1"
	planeDedicatedGateway = "mtls-tunnel-2"
)

// TestBackendClientCertPerPlaneEndToEnd pins that each data plane presents its
// own Gateway's client certificate when one route has parents on different
// planes. Gateway A is served by the shared plane on the suite's tunnel and
// presents client A; Gateway B is served by its own plane on a second tunnel
// and presents client B. Each tunnel's hostname reaches only that tunnel's
// connectors, so a request through it can only be served by that plane.
// Both spec orders are covered: a plane that took the route's first parent
// instead of its own would present the wrong certificate in one of them.
func TestBackendClientCertPerPlaneEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)

	secondHostname := os.Getenv(secondTunnelHostnameEnvVar)
	if secondHostname == "" {
		skipWithoutSecondTunnel(t)
	}

	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupMTLSBackend(ctx, t, k8sClient, cfg.TestNamespace)
	setupPerPlaneGateways(ctx, t, k8sClient, cfg)

	sharedParent := gatewayv1.ParentReference{Name: planeSharedGateway}
	dedicatedParent := gatewayv1.ParentReference{Name: planeDedicatedGateway}

	tests := []struct {
		name    string
		path    string
		parents []gatewayv1.ParentReference
	}{
		{name: "shared plane's Gateway first", path: "/mtls-plane-ab", parents: []gatewayv1.ParentReference{
			sharedParent, dedicatedParent,
		}},
		{name: "dedicated plane's Gateway first", path: "/mtls-plane-ba", parents: []gatewayv1.ParentReference{
			dedicatedParent, sharedParent,
		}},
	}

	for _, tt := range tests {
		route := buildMTLSRoute(cfg, tt.path, tt.parents)
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(secondHostname))
		createHTTPRoute(t, k8sClient, route)

		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })

		t.Run(tt.name, func(t *testing.T) {
			for _, plane := range []struct{ host, wantCN string }{
				{host: cfg.TunnelHostname, wantCN: mtlsClientA},
				{host: secondHostname, wantCN: mtlsClientB},
			} {
				waitForClientCertCN(ctx, t, httpClient, plane.host, tt.path, plane.wantCN)

				for range mtlsSamples {
					echo, resp, err := makeRequest(ctx, t, httpClient, plane.host, http.MethodGet, tt.path, nil)
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, resp.StatusCode)
					assert.Equal(t, plane.wantCN, peerCertCommonName(t, echo), "via %s, pod %s", plane.host, echo.Pod)
				}
			}
		})
	}
}

// skipWithoutSecondTunnel skips loudly: the scheduled workflow runs without
// the second tunnel unless its secrets are set, and a quiet skip there would
// hide this test for good.
func skipWithoutSecondTunnel(t *testing.T) {
	t.Helper()

	const msg = "per-plane client certificate e2e NOT RUN: " + secondTunnelHostnameEnvVar +
		" is unset; it needs a second test tunnel (CF_TUNNEL_2_TOKEN and CF_TUNNEL_2_HOSTNAME, see .env.example)"

	if os.Getenv("GITHUB_ACTIONS") == "true" {
		_, _ = fmt.Fprintln(os.Stdout, "::warning title=e2e test skipped::"+msg)
	}

	t.Skip(msg)
}

// setupPerPlaneGateways creates Gateway A on the shared plane presenting
// client A and Gateway B on a plane of its own, connected to the second
// tunnel, presenting client B, and waits for both to be programmed.
func setupPerPlaneGateways(ctx context.Context, t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	var source corev1.Secret
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: secondTunnelTokenSecret, Namespace: cfg.Namespace}, &source),
		"%s is set but the second tunnel's token Secret is missing (created by hack/conformance-setup.sh from CF_TUNNEL_2_TOKEN)",
		secondTunnelHostnameEnvVar)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tunnel-2-token", Namespace: cfg.TestNamespace},
		Data:       source.Data,
	}
	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "mtls-tunnel-2", Namespace: cfg.TestNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: tokenSecret.Name},
			Replicas:             new(int32(1)),
		},
	}

	shared := buildMTLSGateway(cfg.TestNamespace, planeSharedGateway, mtlsClientA)
	dedicated := buildMTLSGateway(cfg.TestNamespace, planeDedicatedGateway, mtlsClientB)
	dedicated.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
		ParametersRef: &gatewayv1.LocalParametersReference{
			Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: gwConfig.Name,
		},
	}

	for _, obj := range []client.Object{tokenSecret, gwConfig, shared, dedicated} {
		applyObject(ctx, t, k8sClient, obj)

		t.Cleanup(func() { _ = k8sClient.Delete(context.WithoutCancel(ctx), obj) })
	}

	waitForPerGatewayDeploymentReady(ctx, t, k8sClient,
		types.NamespacedName{Name: "cf-proxy-" + planeDedicatedGateway, Namespace: cfg.TestNamespace})

	for _, gateway := range []*gatewayv1.Gateway{shared, dedicated} {
		waitForGatewayProgrammed(ctx, t, k8sClient, types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace})
	}
}
