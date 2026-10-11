//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	portSchemeTLSGateway   = "port-scheme-tls"
	portSchemePlainGateway = "port-scheme-plain"
	portSchemePath         = "/port-scheme"
	portSchemeRedirectPath = "/port-scheme-redirect"
	portSchemeAltPort      = 2053
)

// TestListenerPortAndSchemeThroughEdge pins what the Cloudflare edge hands
// the proxy and how listeners are matched by it, on the real tunnel hostname.
// One Gateway has HTTPS listeners on 8443 and 2053, another an HTTP listener
// on 8443 whose route has the same path, is older and sorts first, so it
// would win if the scheme were ignored:
//
//   - a request on 8443 reaches the HTTPS 8443 route, and the backend sees
//     the port in Host and X-Forwarded-Proto: https, even when the client
//     sends X-Forwarded-Proto: http, which the edge overwrites,
//   - a request on 2053 reaches the 2053 route and one on 443, which neither
//     Gateway has a listener for, gets a 404,
//   - a scheme-less, port-less redirect keeps https and 8443,
//   - a request connected to 443 that names 8443 in Host reaches the 8443
//     route: the port is the client's to choose, not an access boundary.
//
// The test zone has Always Use HTTPS on, which answers plain HTTP on every
// port with a redirect to https on 443 before the tunnel, so an HTTP listener
// is never reached over plain HTTP here and X-Forwarded-Proto: http is never
// observed live.
func TestListenerPortAndSchemeThroughEdge(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)

	tlsGateway := buildPortSchemeGateway(cfg, portSchemeTLSGateway,
		portSchemeListener("https-8443", listenerSetEntryPort, gatewayv1.HTTPSProtocolType, cfg.TunnelHostname),
		portSchemeListener("https-2053", portSchemeAltPort, gatewayv1.HTTPSProtocolType, cfg.TunnelHostname),
	)
	plainGateway := buildPortSchemeGateway(cfg, portSchemePlainGateway,
		portSchemeListener("http-8443", listenerSetEntryPort, gatewayv1.HTTPProtocolType, cfg.TunnelHostname),
	)

	for _, gateway := range []*gatewayv1.Gateway{tlsGateway, plainGateway} {
		applyObject(ctx, t, k8sClient, gateway)

		t.Cleanup(func() { _ = k8sClient.Delete(context.WithoutCancel(ctx), gateway) })

		waitForGatewayProgrammed(ctx, t, k8sClient, types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace})
	}

	plainRoute := buildPortSchemeRoute(cfg, "port-scheme-a-plain", portSchemePlainGateway, "http-8443",
		portSchemeEchoRule(portSchemePath, "echo-v2"))
	createPortSchemeRoute(t, k8sClient, plainRoute)
	waitForRouteAcceptedReason(t, k8sClient, plainRoute, metav1.ConditionTrue, "",
		"the HTTP 8443 route must be served for the scheme check to mean anything")
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has second resolution
	createPortSchemeRoute(t, k8sClient, buildPortSchemeRoute(cfg, "port-scheme-b-tls", portSchemeTLSGateway, "https-8443",
		portSchemeEchoRule(portSchemePath, "echo-v1"), portSchemeRedirectRule()))
	createPortSchemeRoute(t, k8sClient, buildPortSchemeRoute(cfg, "port-scheme-c-alt", portSchemeTLSGateway, "https-2053",
		portSchemeEchoRule(portSchemePath, "echo-v3")))

	onTLSPort := fmt.Sprintf("%s:%d", cfg.TunnelHostname, listenerSetEntryPort)
	onAltPort := fmt.Sprintf("%s:%d", cfg.TunnelHostname, portSchemeAltPort)

	waitForBackend(t, httpClient, onTLSPort, portSchemePath, "echo-v1-", 90*time.Second)
	waitForBackend(t, httpClient, onAltPort, portSchemePath, "echo-v3-", 90*time.Second)

	echo, resp, err := makeRequest(ctx, t, httpClient, onTLSPort, http.MethodGet, portSchemePath,
		map[string]string{"X-Forwarded-Proto": "http"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "the HTTP 8443 listener must not take an HTTPS request, got %s", echo.Pod)
	assert.Equal(t, onTLSPort, echo.Host, "the edge keeps a non-default port in Host")
	assert.Equal(t, []string{"https"}, echo.Headers["X-Forwarded-Proto"], "the edge sets the scheme and overwrites the client's")

	_, resp, err = makeRequest(ctx, t, httpClient, cfg.TunnelHostname, http.MethodGet, portSchemePath, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no listener of either Gateway is on 443")

	_, resp, err = makeRequest(ctx, t, httpClient, onTLSPort, http.MethodGet, portSchemeRedirectPath, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "https://"+onTLSPort+"/redirected", resp.Header.Get("Location"))

	pod := podWithHost(ctx, t, httpClient, cfg.TunnelHostname, onTLSPort)
	assert.True(t, strings.HasPrefix(pod, "echo-v1-"), "a Host naming 8443 on a connection to 443 is matched on 8443, got %q", pod)
}

func portSchemeListener(name string, port gatewayv1.PortNumber, protocol gatewayv1.ProtocolType, hostname string) gatewayv1.Listener {
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Port:     port,
		Protocol: protocol,
		Hostname: new(gatewayv1.Hostname(hostname)),
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: new(gatewayv1.NamespacesFromSame)},
		},
	}
}

func buildPortSchemeGateway(cfg testConfig, name string, listeners ...gatewayv1.Listener) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.TestNamespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        listeners,
		},
	}
}

func portSchemeEchoRule(path, backend string) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix(path)}},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRef(backend, 80, nil)},
	}
}

func portSchemeRedirectRule() gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix(portSchemeRedirectPath)}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: new("/redirected"),
				},
				StatusCode: new(http.StatusFound),
			},
		}},
	}
}

func buildPortSchemeRoute(cfg testConfig, name, gateway, section string, rules ...gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
	route := buildHTTPRoute(name, cfg, rules)
	route.Spec.ParentRefs = []gatewayv1.ParentReference{{
		Name:        gatewayv1.ObjectName(gateway),
		SectionName: new(gatewayv1.SectionName(section)),
	}}

	return route
}

func createPortSchemeRoute(t *testing.T, k8sClient client.Client, route *gatewayv1.HTTPRoute) {
	t.Helper()

	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })
}

// podWithHost connects to connectHost and sends hostHeader as Host,
// returning the answering echo pod ("" when the request was not answered with
// 200).
func podWithHost(ctx context.Context, t *testing.T, httpClient *http.Client, connectHost, hostHeader string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+connectHost+portSchemePath, nil)
	require.NoError(t, err)

	req.Host = hostHeader

	resp, err := doWithTransportRetry(httpClient, req)
	require.NoError(t, err)

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var echo echoResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&echo))

	return echo.Pod
}
