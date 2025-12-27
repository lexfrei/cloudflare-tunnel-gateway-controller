//go:build conformance

package conformance

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	testDomain     = "cf-test.lex.la"
	testNamespace  = "gateway-conformance-infra"
	testGateway    = "all-namespaces" // Uses Gateway with working AWG sidecar
	requestTimeout = 30 * time.Second
	setupTimeout   = 120 * time.Second
	tunnelSyncWait = 10 * time.Second // Wait for Cloudflare tunnel to sync
	maxRetries     = 10               // Max retries for HTTP requests
	retryInterval  = 2 * time.Second  // Interval between retries
)

func setupClient(t *testing.T) client.Client {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	require.NoError(t, err, "failed to load kubeconfig")

	c, err := client.New(config, client.Options{})
	require.NoError(t, err, "failed to create client")

	err = gatewayv1.Install(c.Scheme())
	require.NoError(t, err, "failed to add gateway scheme")

	return c
}

func createHTTPRoute(t *testing.T, c client.Client, name, hostname, backendName string, port int32) {
	t.Helper()

	ns := gatewayv1.Namespace(testNamespace)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      gatewayv1.ObjectName(testGateway),
					Namespace: &ns,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(backendName),
							Port: (*gatewayv1.PortNumber)(&port),
						},
					},
				}},
			}},
		},
	}

	err := c.Create(context.Background(), route)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		require.NoError(t, err, "failed to create HTTPRoute")
	}

	t.Cleanup(func() {
		_ = c.Delete(context.Background(), route)
	})
}

func createHTTPRouteWithPath(t *testing.T, c client.Client, name, hostname, path string, pathType gatewayv1.PathMatchType, backendName string, port int32) {
	t.Helper()

	ns := gatewayv1.Namespace(testNamespace)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      gatewayv1.ObjectName(testGateway),
					Namespace: &ns,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  &pathType,
						Value: &path,
					},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(backendName),
							Port: (*gatewayv1.PortNumber)(&port),
						},
					},
				}},
			}},
		},
	}

	err := c.Create(context.Background(), route)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		require.NoError(t, err, "failed to create HTTPRoute")
	}

	t.Cleanup(func() {
		_ = c.Delete(context.Background(), route)
	})
}

func waitForRoute(t *testing.T, c client.Client, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for route %s to be accepted", name)
		default:
			var route gatewayv1.HTTPRoute
			err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &route)
			if err == nil {
				for _, parent := range route.Status.Parents {
					for _, cond := range parent.Conditions {
						if cond.Type == "Accepted" && cond.Status == metav1.ConditionTrue {
							time.Sleep(tunnelSyncWait) // Wait for tunnel sync
							return
						}
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func makeRequest(t *testing.T, method, url string) (int, string, map[string][]string) {
	t.Helper()

	httpClient := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // testing only
			},
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	require.NoError(t, err)

	resp, err := httpClient.Do(req)
	require.NoError(t, err, "request to %s failed", url)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body), resp.Header
}

// makeRequestExpecting retries until expected status and body content are received.
// This handles Cloudflare CDN propagation delays and tunnel sync timing.
func makeRequestExpecting(t *testing.T, method, url string, expectedStatus int, expectedBody string) (int, string) {
	t.Helper()

	httpClient := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // testing only
			},
		},
	}

	var lastStatus int
	var lastBody string

	for i := range maxRetries {
		req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
		require.NoError(t, err)

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Logf("retry %d/%d: request failed: %v", i+1, maxRetries, err)
			time.Sleep(retryInterval)

			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			t.Logf("retry %d/%d: read body failed: %v", i+1, maxRetries, err)
			time.Sleep(retryInterval)

			continue
		}

		lastStatus = resp.StatusCode
		lastBody = string(body)

		if lastStatus == expectedStatus && strings.Contains(lastBody, expectedBody) {
			return lastStatus, lastBody
		}

		t.Logf("retry %d/%d: got status %d (want %d), body contains %q: %v",
			i+1, maxRetries, lastStatus, expectedStatus, expectedBody, strings.Contains(lastBody, expectedBody))
		time.Sleep(retryInterval)
	}

	return lastStatus, lastBody
}

func TestCFTunnel_SimpleRoute(t *testing.T) {
	c := setupClient(t)

	createHTTPRouteWithPath(t, c, "cf-simple", testDomain, "/simple", gatewayv1.PathMatchPathPrefix, "infra-backend-v1", 8080)
	waitForRoute(t, c, "cf-simple")

	status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/simple", testDomain), 200, "infra-backend-v1")

	assert.Equal(t, 200, status)
	assert.Contains(t, body, "infra-backend-v1")
}

func TestCFTunnel_PathPrefixMatch(t *testing.T) {
	c := setupClient(t)

	createHTTPRouteWithPath(t, c, "cf-path-v1", testDomain, "/prefix-api", gatewayv1.PathMatchPathPrefix, "infra-backend-v1", 8080)
	createHTTPRouteWithPath(t, c, "cf-path-v2", testDomain, "/prefix-api/v2", gatewayv1.PathMatchPathPrefix, "infra-backend-v2", 8080)
	waitForRoute(t, c, "cf-path-v1")
	waitForRoute(t, c, "cf-path-v2")

	t.Run("shorter prefix matches v1", func(t *testing.T) {
		status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/prefix-api/something", testDomain), 200, "infra-backend-v1")
		assert.Equal(t, 200, status)
		assert.Contains(t, body, "infra-backend-v1")
	})

	t.Run("longer prefix matches v2", func(t *testing.T) {
		status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/prefix-api/v2/something", testDomain), 200, "infra-backend-v2")
		assert.Equal(t, 200, status)
		assert.Contains(t, body, "infra-backend-v2")
	})
}

func TestCFTunnel_ExactPathMatch(t *testing.T) {
	c := setupClient(t)

	createHTTPRouteWithPath(t, c, "cf-exact", testDomain, "/exactmatch", gatewayv1.PathMatchExact, "infra-backend-v3", 8080)
	createHTTPRouteWithPath(t, c, "cf-prefix", testDomain, "/exactmatch", gatewayv1.PathMatchPathPrefix, "infra-backend-v1", 8080)
	waitForRoute(t, c, "cf-exact")
	waitForRoute(t, c, "cf-prefix")

	t.Run("exact path matches exact route", func(t *testing.T) {
		status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/exactmatch", testDomain), 200, "infra-backend-v3")
		assert.Equal(t, 200, status)
		assert.Contains(t, body, "infra-backend-v3")
	})

	t.Run("prefix path matches prefix route", func(t *testing.T) {
		status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/exactmatch/subpath", testDomain), 200, "infra-backend-v1")
		assert.Equal(t, 200, status)
		assert.Contains(t, body, "infra-backend-v1")
	})
}

func TestCFTunnel_MultipleBackends(t *testing.T) {
	c := setupClient(t)

	createHTTPRouteWithPath(t, c, "cf-multi-1", testDomain, "/multi-v1", gatewayv1.PathMatchPathPrefix, "infra-backend-v1", 8080)
	createHTTPRouteWithPath(t, c, "cf-multi-2", testDomain, "/multi-v2", gatewayv1.PathMatchPathPrefix, "infra-backend-v2", 8080)
	createHTTPRouteWithPath(t, c, "cf-multi-3", testDomain, "/multi-v3", gatewayv1.PathMatchPathPrefix, "infra-backend-v3", 8080)
	waitForRoute(t, c, "cf-multi-1")
	waitForRoute(t, c, "cf-multi-2")
	waitForRoute(t, c, "cf-multi-3")

	tests := []struct {
		path    string
		backend string
	}{
		{"/multi-v1/test", "infra-backend-v1"},
		{"/multi-v2/test", "infra-backend-v2"},
		{"/multi-v3/test", "infra-backend-v3"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s%s", testDomain, tc.path), 200, tc.backend)
			assert.Equal(t, 200, status)
			assert.Contains(t, body, tc.backend)
		})
	}
}

func TestCFTunnel_CatchAll404(t *testing.T) {
	c := setupClient(t)

	// Create route for specific path only
	createHTTPRouteWithPath(t, c, "cf-catchall", testDomain, "/catchall-exists", gatewayv1.PathMatchExact, "infra-backend-v1", 8080)
	waitForRoute(t, c, "cf-catchall")

	t.Run("existing path returns 200", func(t *testing.T) {
		status, body := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/catchall-exists", testDomain), 200, "infra-backend-v1")
		assert.Equal(t, 200, status)
		assert.Contains(t, body, "infra-backend-v1")
	})

	t.Run("non-existing path returns 404", func(t *testing.T) {
		status, _ := makeRequestExpecting(t, "GET", fmt.Sprintf("https://%s/catchall-not-found-xyz", testDomain), 404, "")
		assert.Equal(t, 404, status)
	})
}
