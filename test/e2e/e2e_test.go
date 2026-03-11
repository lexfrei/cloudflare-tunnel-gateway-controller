//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// echoResponse mirrors the JSON structure returned by the Gateway API conformance
// echo-basic server (gcr.io/k8s-staging-gateway-api/echo-basic).
type echoResponse struct {
	Path      string              `json:"path"`
	Host      string              `json:"host"`
	Method    string              `json:"method"`
	Protocol  string              `json:"proto"`
	Headers   map[string][]string `json:"headers"`
	Namespace string              `json:"namespace"`
	Pod       string              `json:"pod"`
}

// testConfig holds environment-driven test configuration.
type testConfig struct {
	TunnelHostname string
	KubeContext    string
	Namespace      string
	TestNamespace  string
	GatewayName    string
}

func loadTestConfig() testConfig {
	return testConfig{
		TunnelHostname: envOrDefault("CONFORMANCE_TUNNEL_HOSTNAME", "v2-test.lex.la"),
		KubeContext:    envOrDefault("CONFORMANCE_KUBE_CONTEXT", "kind-v2-test"),
		Namespace:      envOrDefault("CONFORMANCE_NAMESPACE", "cloudflare-tunnel-system"),
		TestNamespace:  envOrDefault("CONFORMANCE_TEST_NAMESPACE", "conformance-test"),
		GatewayName:    envOrDefault("CONFORMANCE_GATEWAY_NAME", "conformance-gateway"),
	}
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultVal
}

// tunnelClient creates an HTTP client for Cloudflare tunnel requests.
func tunnelClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// makeRequest sends a request through the Cloudflare tunnel and parses the
// echo-basic JSON response.
func makeRequest(
	t *testing.T,
	httpClient *http.Client,
	tunnelHostname string,
	method string,
	path string,
	headers map[string]string,
) (*echoResponse, *http.Response, error) {
	t.Helper()

	reqURL := fmt.Sprintf("https://%s%s", tunnelHostname, path)

	req, err := http.NewRequestWithContext(context.Background(), method, reqURL, nil)
	if err != nil {
		return nil, nil, err
	}

	for key, val := range headers {
		req.Header.Set(key, val)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp, err
	}

	var echo echoResponse
	if resp.Header.Get("Content-Type") == "application/json" {
		if jsonErr := json.Unmarshal(body, &echo); jsonErr != nil {
			return nil, resp, fmt.Errorf("failed to parse echo response: %w (body: %s)", jsonErr, string(body))
		}
	} else {
		echo.Method = method
	}

	return &echo, resp, nil
}

// waitForBackend polls until the given path routes to a pod matching podPrefix.
func waitForBackend(
	t *testing.T,
	httpClient *http.Client,
	tunnelHostname string,
	path string,
	podPrefix string,
	timeout time.Duration,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, timeout, true,
		func(_ context.Context) (bool, error) {
			echo, resp, reqErr := makeRequest(t, httpClient, tunnelHostname, http.MethodGet, path, nil)
			if reqErr != nil {
				return false, nil
			}

			if resp.StatusCode == http.StatusNotFound {
				return false, nil
			}

			return strings.HasPrefix(echo.Pod, podPrefix), nil
		},
	)
	require.NoError(t, err, "timed out waiting for %s to route to %s*", path, podPrefix)
}

// deleteAllRoutes removes all HTTPRoutes in the test namespace and waits for proxy to update.
func deleteAllRoutes(t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	ctx := context.Background()
	routeList := &gatewayv1.HTTPRouteList{}

	err := k8sClient.List(ctx, routeList, client.InNamespace(cfg.TestNamespace))
	if err != nil {
		return
	}

	for idx := range routeList.Items {
		_ = k8sClient.Delete(ctx, &routeList.Items[idx])
	}

	// Wait for proxy to clear old routes.
	if len(routeList.Items) > 0 {
		time.Sleep(3 * time.Second)
	}
}

// TestHTTPRouteConformance runs Gateway API HTTPRoute conformance-style tests
// against a live Cloudflare Tunnel deployment.
//
// Run with: go test -v -tags e2e -timeout 10m ./test/e2e/
func TestHTTPRouteConformance(t *testing.T) {
	cfg := loadTestConfig()
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	// Clean slate.
	deleteAllRoutes(t, k8sClient, cfg)

	t.Run("Core", func(t *testing.T) {
		t.Run("HTTPRouteSimpleSameNamespace", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteSimpleSameNamespace(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRoutePathPrefixMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRoutePathPrefixMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteExactPathMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteExactPathMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteMatchingAcrossRoutes", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteMatchingAcrossRoutes(t, httpClient, k8sClient, cfg)
		})
	})

	t.Run("Extended", func(t *testing.T) {
		t.Run("HTTPRouteHeaderMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteHeaderMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteMethodMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteMethodMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteQueryParamMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteQueryParamMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteWeight", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteWeight(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRequestHeaderModifier", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRequestHeaderModifier(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteResponseHeaderModifier", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteResponseHeaderModifier(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRequestRedirect", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRequestRedirect(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRegexPathMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRegexPathMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRegexHeaderMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRegexHeaderMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRegexQueryParamMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRegexQueryParamMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRoutePathMatchOrder", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRoutePathMatchOrder(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteURLRewritePath", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteURLRewritePath(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteURLRewriteHost", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteURLRewriteHost(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRequestMirror", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRequestMirror(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRedirectPort", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRedirectPort(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteRedirectPath", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteRedirectPath(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteCombinedMatching", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteCombinedMatching(t, httpClient, k8sClient, cfg)
		})

		t.Run("HTTPRouteMultipleMatchesOR", func(t *testing.T) {
			defer deleteAllRoutes(t, k8sClient, cfg)
			testHTTPRouteMultipleMatchesOR(t, httpClient, k8sClient, cfg)
		})
	})

	t.Run("Gateway", func(t *testing.T) {
		t.Run("GatewayAcceptedCondition", func(t *testing.T) {
			testGatewayAcceptedCondition(t, k8sClient, cfg)
		})

		t.Run("GatewayObservedGenerationBump", func(t *testing.T) {
			testGatewayObservedGenerationBump(t, k8sClient, cfg)
		})
	})
}

// --- Core conformance tests ---

func testHTTPRouteSimpleSameNamespace(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("simple-same-ns", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{Path: pathPrefix("/simple")},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{
				backendRef("echo-v1", 80, nil),
			},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/simple", "echo-v1-", 60*time.Second)

	echo, resp, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/simple", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "/simple", echo.Path)
	assert.Equal(t, cfg.TestNamespace, echo.Namespace)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "pod should be echo-v1, got: %s", echo.Pod)

	echo, resp, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/simple/sub", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "/simple/sub", echo.Path)
}

func testHTTPRoutePathPrefixMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("path-prefix", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/prefix-a")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/prefix-b")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/prefix-a", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/prefix-a", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"))

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/prefix-b", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"))

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/prefix-a/sub", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"))
}

func testHTTPRouteExactPathMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("exact-path", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathExact("/exact-only")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/exact")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/exact-only", "echo-v1-", 60*time.Second)

	// Exact match → echo-v1.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/exact-only", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "exact match → echo-v1, got: %s", echo.Pod)

	// Cloudflare Tunnel exact-match behaviour is not fully predictable for sub-paths:
	// the tunnel may return 404 or route to either backend depending on internal
	// ingress rule ordering.  We only verify the sub-path does NOT reach echo-v2,
	// which would indicate the prefix rule incorrectly won over the exact rule.
	echo, resp, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/exact-only/sub", nil)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(echo.Pod, "echo-v2-"),
		"sub-path must NOT match prefix /exact → echo-v2 (status %d, pod: %s)", resp.StatusCode, echo.Pod)
}

func testHTTPRouteMatchingAcrossRoutes(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route1 := buildHTTPRoute("across-a", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/across/route-a")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})
	route2 := buildHTTPRoute("across-b", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/across/route-b")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route1)
	createHTTPRoute(t, k8sClient, route2)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/across/route-a", "echo-v1-", 60*time.Second)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/across/route-b", "echo-v2-", 30*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/across/route-a", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"))

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/across/route-b", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"))
}

// --- Extended conformance tests ---

func testHTTPRouteHeaderMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	headerMatchType := gatewayv1.HeaderMatchExact

	route := buildHTTPRoute("header-match", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path: pathPrefix("/hdr-test"),
					Headers: []gatewayv1.HTTPHeaderMatch{
						{Type: &headerMatchType, Name: "X-Test-Backend", Value: "v2"},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/hdr-test")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/hdr-test", "echo-v1-", 60*time.Second)

	// Without header → echo-v1.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/hdr-test", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "no header → echo-v1, got: %s", echo.Pod)

	// With matching header → echo-v2.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/hdr-test",
		map[string]string{"X-Test-Backend": "v2"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "matching header → echo-v2, got: %s", echo.Pod)

	// Non-matching header → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/hdr-test",
		map[string]string{"X-Test-Backend": "v1"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "wrong header → echo-v1, got: %s", echo.Pod)
}

func testHTTPRouteMethodMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	methodPost := gatewayv1.HTTPMethodPost
	methodGet := gatewayv1.HTTPMethodGet

	route := buildHTTPRoute("method-match", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{Path: pathPrefix("/meth-test"), Method: &methodPost},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{Path: pathPrefix("/meth-test"), Method: &methodGet},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/meth-test", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/meth-test", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "GET → echo-v1, got: %s", echo.Pod)

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodPost, "/meth-test", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "POST → echo-v2, got: %s", echo.Pod)
}

func testHTTPRouteQueryParamMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	queryMatchExact := gatewayv1.QueryParamMatchExact

	route := buildHTTPRoute("query-match", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path: pathPrefix("/qp-test"),
					QueryParams: []gatewayv1.HTTPQueryParamMatch{
						{Type: &queryMatchExact, Name: "version", Value: "v2"},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/qp-test")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/qp-test", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/qp-test", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "no qp → echo-v1, got: %s", echo.Pod)

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/qp-test?version=v2", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "matching qp → echo-v2, got: %s", echo.Pod)

	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/qp-test?version=v1", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "wrong qp → echo-v1, got: %s", echo.Pod)
}

func testHTTPRouteWeight(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	weight90 := int32(90)
	weight10 := int32(10)

	route := buildHTTPRoute("weight-split", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/wt-test")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{
				backendRef("echo-v1", 80, &weight90),
				backendRef("echo-v2", 80, &weight10),
			},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/wt-test", "echo-v", 60*time.Second)

	const total = 30

	v1, v2 := 0, 0

	for range total {
		echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/wt-test", nil)
		require.NoError(t, err)

		switch {
		case strings.HasPrefix(echo.Pod, "echo-v1-"):
			v1++
		case strings.HasPrefix(echo.Pod, "echo-v2-"):
			v2++
		default:
			t.Fatalf("unexpected pod: %s", echo.Pod)
		}
	}

	t.Logf("weight distribution: v1=%d, v2=%d (total=%d)", v1, v2, total)
	assert.GreaterOrEqual(t, v1, total/2, "echo-v1 (w=90) should get majority")
	assert.GreaterOrEqual(t, v2, 1, "echo-v2 (w=10) should get at least 1 request")
}

func testHTTPRouteRequestHeaderModifier(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("req-hdr-mod", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/req-hdr-mod")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
					RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set: []gatewayv1.HTTPHeader{
							{Name: "X-Custom-Set", Value: "set-value"},
						},
						Add: []gatewayv1.HTTPHeader{
							{Name: "X-Custom-Add", Value: "add-value"},
						},
						Remove: []string{"X-Remove-Me"},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/req-hdr-mod", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/req-hdr-mod",
		map[string]string{"X-Remove-Me": "should-be-removed"})
	require.NoError(t, err)

	// Echo-basic echoes headers as received by the backend.
	// The proxy applies header modifications before forwarding.
	assert.Contains(t, echo.Headers, "X-Custom-Set", "set header should be present")
	if vals, ok := echo.Headers["X-Custom-Set"]; ok {
		assert.Contains(t, vals, "set-value")
	}

	assert.Contains(t, echo.Headers, "X-Custom-Add", "add header should be present")
	if vals, ok := echo.Headers["X-Custom-Add"]; ok {
		assert.Contains(t, vals, "add-value")
	}

	assert.NotContains(t, echo.Headers, "X-Remove-Me", "removed header should not be present")
}

func testHTTPRouteResponseHeaderModifier(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("resp-hdr-mod", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/resp-hdr-mod")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
					ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set: []gatewayv1.HTTPHeader{
							{Name: "X-Response-Set", Value: "response-value"},
						},
						Add: []gatewayv1.HTTPHeader{
							{Name: "X-Response-Add", Value: "added-value"},
						},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/resp-hdr-mod", "echo-v1-", 60*time.Second)

	_, resp, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/resp-hdr-mod", nil)
	require.NoError(t, err)

	// Response headers may be modified by Cloudflare CDN.
	// The proxy adds these headers to the backend response before returning.
	assert.Equal(t, "response-value", resp.Header.Get("X-Response-Set"), "response set header")
	assert.Equal(t, "added-value", resp.Header.Get("X-Response-Add"), "response add header")
}

func testHTTPRouteRequestRedirect(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	statusCode := 301

	route := buildHTTPRoute("redirect", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/redir-test")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Scheme:     new("https"),
						Hostname:   new(gatewayv1.PreciseHostname("redirected.example.com")),
						StatusCode: &statusCode,
					},
				},
			},
		},
	})

	createHTTPRoute(t, k8sClient, route)

	// Wait for 301 response (redirect has no echo body).
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 60*time.Second, true,
		func(_ context.Context) (bool, error) {
			_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-test", nil)
			if reqErr != nil {
				return false, nil
			}

			return resp.StatusCode == http.StatusMovedPermanently, nil
		},
	)
	require.NoError(t, err, "redirect route did not return 301")

	_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-test", nil)
	require.NoError(t, reqErr)
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.Contains(t, location, "redirected.example.com")
	assert.Contains(t, location, "https://")
}

// --- Helpers ---

func pathPrefix(path string) *gatewayv1.HTTPPathMatch {
	t := gatewayv1.PathMatchPathPrefix
	return &gatewayv1.HTTPPathMatch{Type: &t, Value: &path}
}

func pathExact(path string) *gatewayv1.HTTPPathMatch {
	t := gatewayv1.PathMatchExact
	return &gatewayv1.HTTPPathMatch{Type: &t, Value: &path}
}

func pathRegex(pattern string) *gatewayv1.HTTPPathMatch {
	t := gatewayv1.PathMatchRegularExpression
	return &gatewayv1.HTTPPathMatch{Type: &t, Value: &pattern}
}

func backendRef(name string, port int32, weight *int32) gatewayv1.HTTPBackendRef {
	p := gatewayv1.PortNumber(port)

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &p,
			},
			Weight: weight,
		},
	}
}

func buildHTTPRoute(name string, cfg testConfig, rules []gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
	gatewayNs := gatewayv1.Namespace(cfg.Namespace)
	hostname := gatewayv1.Hostname(cfg.TunnelHostname)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.TestNamespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      gatewayv1.ObjectName(cfg.GatewayName),
						Namespace: &gatewayNs,
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{hostname},
			Rules:     rules,
		},
	}
}

func createHTTPRoute(t *testing.T, k8sClient client.Client, route *gatewayv1.HTTPRoute) {
	t.Helper()

	ctx := context.Background()
	existing := &gatewayv1.HTTPRoute{}

	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      route.Name,
		Namespace: route.Namespace,
	}, existing)
	if err == nil {
		require.NoError(t, k8sClient.Delete(ctx, existing))
		time.Sleep(time.Second)
	}

	require.NoError(t, k8sClient.Create(ctx, route))
	t.Logf("created HTTPRoute %s/%s", route.Namespace, route.Name)
}

func setupTestNamespace(t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.TestNamespace}}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: cfg.TestNamespace}, ns)
	if err != nil {
		require.NoError(t, k8sClient.Create(ctx, ns))
		t.Logf("created namespace %s", cfg.TestNamespace)
	}
}

// --- Extended conformance tests (regex, rewrite, mirror, redirect, combined) ---

func testHTTPRouteRegexPathMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("regex-path", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathRegex(`/rgx/item-[0-9]+`)}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/rgx")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/rgx/other", "echo-v2-", 60*time.Second)

	// Regex match → echo-v1.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx/item-42", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "regex match → echo-v1, got: %s", echo.Pod)

	// Non-numeric suffix → falls to prefix → echo-v2.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx/item-abc", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "no regex match → echo-v2, got: %s", echo.Pod)

	// Other path under /rgx → prefix fallback → echo-v2.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx/other", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "prefix fallback → echo-v2, got: %s", echo.Pod)
}

func testHTTPRouteRegexHeaderMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	headerMatchRegex := gatewayv1.HeaderMatchRegularExpression

	route := buildHTTPRoute("regex-header", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path: pathPrefix("/rgx-hdr"),
					Headers: []gatewayv1.HTTPHeaderMatch{
						{Type: &headerMatchRegex, Name: "X-Api-Version", Value: `v[0-9]+`},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/rgx-hdr")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/rgx-hdr", "echo-v1-", 60*time.Second)

	// Header matching regex → echo-v2.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-hdr",
		map[string]string{"X-Api-Version": "v2"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "regex header match → echo-v2, got: %s", echo.Pod)

	// Header not matching regex → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-hdr",
		map[string]string{"X-Api-Version": "latest"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "non-matching header → echo-v1, got: %s", echo.Pod)

	// No header → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-hdr", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "no header → echo-v1, got: %s", echo.Pod)
}

func testHTTPRouteRegexQueryParamMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	queryMatchRegex := gatewayv1.QueryParamMatchRegularExpression

	route := buildHTTPRoute("regex-query", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path: pathPrefix("/rgx-qp"),
					QueryParams: []gatewayv1.HTTPQueryParamMatch{
						{Type: &queryMatchRegex, Name: "id", Value: `[0-9]+`},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/rgx-qp")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/rgx-qp", "echo-v1-", 60*time.Second)

	// Numeric query param → echo-v2.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-qp?id=123", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "regex qp match → echo-v2, got: %s", echo.Pod)

	// Non-numeric query param → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-qp?id=abc", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "non-matching qp → echo-v1, got: %s", echo.Pod)

	// No query param → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rgx-qp", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "no qp → echo-v1, got: %s", echo.Pod)
}

func testHTTPRoutePathMatchOrder(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("path-order", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathExact("/pmo/specific")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/pmo")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/pmo/other", "echo-v2-", 60*time.Second)

	// Exact match wins → echo-v1.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/pmo/specific", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "exact wins → echo-v1, got: %s", echo.Pod)

	// Prefix fallback → echo-v2.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/pmo/other", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "prefix fallback → echo-v2, got: %s", echo.Pod)
}

func testHTTPRouteURLRewritePath(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("url-rewrite-path", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/rewrite-test")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
						Path: &gatewayv1.HTTPPathModifier{
							Type:            gatewayv1.FullPathHTTPPathModifier,
							ReplaceFullPath: new("/replaced"),
						},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/rewrite-test", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rewrite-test/anything", nil)
	require.NoError(t, err)
	assert.Equal(t, "/replaced", echo.Path, "path should be rewritten to /replaced")
}

func testHTTPRouteURLRewriteHost(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	rewrittenHost := gatewayv1.PreciseHostname("rewritten.internal")

	route := buildHTTPRoute("url-rewrite-host", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/rewrite-host")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
						Hostname: &rewrittenHost,
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/rewrite-host", "echo-v1-", 60*time.Second)

	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/rewrite-host", nil)
	require.NoError(t, err)
	assert.Equal(t, "rewritten.internal", echo.Host, "host should be rewritten to rewritten.internal")
}

func testHTTPRouteRequestMirror(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	mirrorPort := gatewayv1.PortNumber(80)

	route := buildHTTPRoute("mirror-test", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/mirror-test")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestMirror,
					RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
						BackendRef: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName("echo-v3"),
							Port: &mirrorPort,
						},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/mirror-test", "echo-v1-", 60*time.Second)

	// Response should come from echo-v1 (mirror is fire-and-forget).
	echo, resp, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/mirror-test", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "response from primary → echo-v1, got: %s", echo.Pod)
}

func testHTTPRouteRedirectPort(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	statusCode := 302
	redirectPort := gatewayv1.PortNumber(8443)

	route := buildHTTPRoute("redir-port", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/redir-port")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Port:       &redirectPort,
						StatusCode: &statusCode,
					},
				},
			},
		},
	})

	createHTTPRoute(t, k8sClient, route)

	// Wait for redirect response.
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 60*time.Second, true,
		func(_ context.Context) (bool, error) {
			_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-port", nil)
			if reqErr != nil {
				return false, nil //nolint:nilerr // transient errors are expected during polling
			}

			return resp.StatusCode == http.StatusFound, nil
		},
	)
	require.NoError(t, err, "redirect route did not return 302")

	_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-port", nil)
	require.NoError(t, reqErr)
	assert.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.Contains(t, location, ":8443", "Location should contain port 8443")
}

func testHTTPRouteRedirectPath(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	statusCode := 301

	route := buildHTTPRoute("redir-path", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/redir-path")}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Path: &gatewayv1.HTTPPathModifier{
							Type:            gatewayv1.FullPathHTTPPathModifier,
							ReplaceFullPath: new("/new-dest"),
						},
						StatusCode: &statusCode,
					},
				},
			},
		},
	})

	createHTTPRoute(t, k8sClient, route)

	// Wait for redirect response.
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 60*time.Second, true,
		func(_ context.Context) (bool, error) {
			_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-path", nil)
			if reqErr != nil {
				return false, nil //nolint:nilerr // transient errors are expected during polling
			}

			return resp.StatusCode == http.StatusMovedPermanently, nil
		},
	)
	require.NoError(t, err, "redirect route did not return 301")

	_, resp, reqErr := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/redir-path", nil)
	require.NoError(t, reqErr)
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.Contains(t, location, "/new-dest", "Location should contain /new-dest")
}

func testHTTPRouteCombinedMatching(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	methodGet := gatewayv1.HTTPMethodGet
	headerMatchExact := gatewayv1.HeaderMatchExact
	queryMatchExact := gatewayv1.QueryParamMatchExact

	route := buildHTTPRoute("combined-match", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path:   pathPrefix("/combo"),
					Method: &methodGet,
					Headers: []gatewayv1.HTTPHeaderMatch{
						{Type: &headerMatchExact, Name: "X-Env", Value: "prod"},
					},
					QueryParams: []gatewayv1.HTTPQueryParamMatch{
						{Type: &queryMatchExact, Name: "format", Value: "json"},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v2", 80, nil)},
		},
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/combo")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/combo", "echo-v1-", 60*time.Second)

	// All 4 conditions met → echo-v2.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/combo?format=json",
		map[string]string{"X-Env": "prod"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "all conditions → echo-v2, got: %s", echo.Pod)

	// Missing header → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/combo?format=json", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "missing header → echo-v1, got: %s", echo.Pod)

	// Missing query param → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/combo",
		map[string]string{"X-Env": "prod"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "missing qp → echo-v1, got: %s", echo.Pod)

	// Wrong method (POST) → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodPost, "/combo?format=json",
		map[string]string{"X-Env": "prod"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "wrong method → echo-v1, got: %s", echo.Pod)
}

func testHTTPRouteMultipleMatchesOR(
	t *testing.T, httpClient *http.Client, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	route := buildHTTPRoute("or-match", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{
				{Path: pathExact("/or-a")},
				{Path: pathExact("/or-b")},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 80, nil)},
		},
	})

	createHTTPRoute(t, k8sClient, route)
	waitForBackend(t, httpClient, cfg.TunnelHostname, "/or-a", "echo-v1-", 60*time.Second)

	// /or-a → echo-v1.
	echo, _, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/or-a", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "/or-a → echo-v1, got: %s", echo.Pod)

	// /or-b → echo-v1.
	echo, _, err = makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/or-b", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v1-"), "/or-b → echo-v1, got: %s", echo.Pod)

	// /or-c → 404 (no match).
	_, resp, err := makeRequest(t, httpClient, cfg.TunnelHostname, http.MethodGet, "/or-c", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "/or-c should be 404")
}

// --- Gateway conformance tests ---

func testGatewayAcceptedCondition(
	t *testing.T, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	ctx := context.Background()
	gw := &gatewayv1.Gateway{}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}, gw)
	require.NoError(t, err, "failed to get gateway")

	found := false

	for _, condition := range gw.Status.Conditions {
		if condition.Type == string(gatewayv1.GatewayConditionAccepted) {
			found = true
			assert.Equal(t, metav1.ConditionTrue, condition.Status,
				"Gateway Accepted condition should be True, got: %s (reason: %s)", condition.Status, condition.Reason)
		}
	}

	assert.True(t, found, "Gateway should have Accepted condition in status")
}

func testGatewayObservedGenerationBump(
	t *testing.T, k8sClient client.Client, cfg testConfig,
) {
	t.Helper()

	ctx := context.Background()
	gw := &gatewayv1.Gateway{}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}, gw)
	require.NoError(t, err, "failed to get gateway")

	generation := gw.Generation

	// Verify that at least one status condition has ObservedGeneration matching Generation.
	matched := false

	for _, condition := range gw.Status.Conditions {
		if condition.ObservedGeneration == generation {
			matched = true

			break
		}
	}

	assert.True(t, matched,
		"at least one status condition should have ObservedGeneration=%d matching Generation=%d",
		generation, generation)
}
