//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestHTTPRouteBackendProtocolWebSocket exercises WebSocket support end-to-end
// through a real Cloudflare Tunnel.
//
// This is the substitute for the upstream conformance test
// `HTTPRouteBackendProtocolWebSocket`, which we cannot run: the upstream test
// dials the Gateway address directly with `golang.org/x/net/websocket.Dial`
// (no RoundTripper hook to inject a custom dialer), and our Gateway address is
// `<tunnel-id>.cfargotunnel.com` whose AAAA records point at Cloudflare's ULA
// (fd10::/8), unreachable from any test runner outside Cloudflare's network.
// Same structural blocker that keeps the gRPC conformance tests skipped.
//
// The test path: client opens `wss://<tunnel hostname>/ws` against Cloudflare
// edge. Cloudflare terminates TLS and forwards a plaintext HTTP/1.1 request
// to cloudflared. cloudflared invokes the proxy's `GatewayOriginProxy`. The
// proxy matches the route, sees a Connection: Upgrade header pair, and -- via
// `Handler.proxyToBackend`'s `shouldUseWebSocketUpgradePath` gate -- routes the
// request to the custom `proxyWebSocketUpgrade` path instead of
// `httputil.ReverseProxy`. The custom path dials the backend itself, parses
// the 101 response, applies route-level ResponseFilters, then writes the 101
// to the client and hijacks. Bytes then flow bidirectionally between the
// test client and the echo-basic backend's `/ws` endpoint. The custom path
// exists because stdlib's `ReverseProxy.handleUpgradeResponse` hijacks
// BEFORE writing the 101 status, which the cloudflared HTTP/2 response
// writer rejects -- the failure mode the production deployment hits.
func TestHTTPRouteBackendProtocolWebSocket(t *testing.T) {
	cfg := loadTestConfig()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)
	deleteAllRoutes(t, k8sClient, cfg)

	// Provision a sibling Service `echo-v1-ws` that selects echo-v1 pods but
	// exposes port 8082 with `appProtocol: kubernetes.io/ws` forwarding to
	// container port 3000 (where echo-basic's WebSocket /ws endpoint lives).
	// Mirrors the upstream conformance manifest's `third-port` shape.
	const wsServiceName = "echo-v1-ws"

	const wsServicePort = int32(8082)

	setupWSBackendService(t, k8sClient, cfg.TestNamespace, wsServiceName, "echo-v1", wsServicePort)
	t.Cleanup(func() { teardownWSBackendService(t, k8sClient, cfg.TestNamespace, wsServiceName) })

	route := buildHTTPRoute("ws-backend", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/ws")}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef(wsServiceName, wsServicePort, nil)},
		},
	})
	createHTTPRoute(t, k8sClient, route)
	t.Cleanup(func() { deleteAllRoutes(t, k8sClient, cfg) })

	// Tunnel config + proxy sync need a moment after the route lands. We
	// don't have a pre-warm HTTP path here (the route only matches /ws),
	// so dial the WebSocket directly with retries until the upgrade
	// succeeds or the timeout elapses.
	remote := fmt.Sprintf("wss://%s/ws", cfg.TunnelHostname)
	origin := fmt.Sprintf("https://%s", cfg.TunnelHostname)

	wsConfig, err := websocket.NewConfig(remote, origin)
	require.NoError(t, err)

	// Cloudflare edge presents its own cert against the tunnel hostname.
	// Reuse the stdlib defaults plus an explicit MinVersion to mirror the
	// HTTPS client elsewhere in this suite.
	wsConfig.TlsConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.TunnelHostname,
	}

	// Manual retry loop instead of require.Eventually so the failure
	// surface includes the actual websocket.DialConfig error message.
	// Eventually swallows the inner error and reports a generic
	// "Condition never satisfied"; for a real-edge e2e where the
	// failure mode is genuinely unknown until you read the dial error,
	// that's the wrong tradeoff. Log per-iteration errors too — this
	// test runs once per PR, so a handful of failure lines isn't spam.
	var (
		conn        *websocket.Conn
		lastDialErr error
	)

	const dialTimeout = 120 * time.Second

	const dialInterval = 3 * time.Second

	deadline := time.Now().Add(dialTimeout)
	for time.Now().Before(deadline) {
		c, dialErr := websocket.DialConfig(wsConfig)
		if dialErr == nil {
			conn = c
			break
		}

		lastDialErr = dialErr
		t.Logf("websocket.DialConfig: %v", dialErr)
		time.Sleep(dialInterval)
	}

	require.NotNil(t, conn,
		"WebSocket upgrade through Cloudflare Tunnel must complete (101 Switching Protocols) — "+
			"tunnel config + proxy sync should converge within %s; last dial error: %v",
		dialTimeout, lastDialErr)

	t.Cleanup(func() { _ = conn.Close() })

	// Text-frame round trip. websocket.Message.Send/Receive frames a UTF-8
	// payload with FrameType 1 (text); echo-basic's /ws handler runs an
	// io.Copy that echoes the same bytes back.
	const textPayload = "hello-tunnel-ws-roundtrip"

	require.NoError(t,
		websocket.Message.Send(conn, textPayload),
		"sending the text frame after the 101 must succeed — conn is hijacked, bytes go to backend")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	var textReply string

	require.NoError(t,
		websocket.Message.Receive(conn, &textReply),
		"receiving the echoed text frame must succeed within the read deadline")

	assert.Equal(t, textPayload, textReply,
		"echo round-trip through Cloudflare edge + tunnel + proxy + echo-basic backend must preserve payload verbatim")
}

// errProxySyncNotConverged exists primarily to satisfy err113's
// "no dynamic errors" rule -- lifts the per-iteration status-code
// surface out of a `fmt.Errorf("got status %d", ...)` literal and
// into a wrapped sentinel. The wrapped status code surfaces in the
// t.Logf line and the final require.NotNil message. Symbolic
// `errors.Is` callers are not needed and not present.
var errProxySyncNotConverged = errors.New("proxy sync not converged yet")

// TestHTTPRouteBackendProtocolWebSocket_AppliesResponseFilters proves end
// to end through the production Cloudflare Tunnel that a route-level
// ResponseHeaderModifier filter applies to the WebSocket 101 response.
// The unit + integration tests cover the contract inside the proxy,
// but only an e2e run can confirm the filter-modified header survives
// cloudflared's HTTP/2 ResponseUserHeaders blob and Cloudflare's edge
// HTTP/1.1 re-serialization back to the client.
//
// Mechanism: stand up a separate ws backend (echo-v1-ws-filtered on
// port 8083) and an HTTPRoute carrying a ResponseHeaderModifier.Add
// filter. Then dial wss://<tunnel hostname>/ws-filtered manually --
// tls.Dial + raw HTTP/1.1 upgrade request -- so the test can read
// the 101 response and inspect the headers Cloudflare delivers. The
// websocket.Dial helper used by the sibling test consumes the 101
// internally and exposes no way to assert on it.
//
// Without ApplyResponseFilters wired into the upgrade path this test
// fails because proxyWebSocketUpgrade used to bypass
// httputil.ReverseProxy.ModifyResponse,
// dropping the filter pipeline on the 101 path -- the client sees the
// backend's original headers (no X-CF-Tunnel-E2E-Added) and the
// assertion fails loudly.
func TestHTTPRouteBackendProtocolWebSocket_AppliesResponseFilters(t *testing.T) {
	cfg := loadTestConfig()
	k8sClient := newK8sClient(t, cfg.KubeContext)

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)
	deleteAllRoutes(t, k8sClient, cfg)

	// Sibling service with a distinct port so the cluster-side state is
	// independent of TestHTTPRouteBackendProtocolWebSocket. The path is
	// /ws because that's where echo-basic serves its WebSocket handler;
	// echo-basic responds 200 for any other path, which would mask the
	// fix this test is meant to pin. Co-existence with the sibling test
	// is safe because each test's deleteAllRoutes cleanup runs before
	// the next test starts.
	const (
		wsServiceName = "echo-v1-ws-filtered"
		wsServicePort = int32(8083)
		filteredPath  = "/ws"
		addedHeader   = "X-Cf-Tunnel-E2e-Added"
		addedValue    = "yes"
	)

	setupWSBackendService(t, k8sClient, cfg.TestNamespace, wsServiceName, "echo-v1", wsServicePort)
	t.Cleanup(func() { teardownWSBackendService(t, k8sClient, cfg.TestNamespace, wsServiceName) })

	headerName := gatewayv1.HTTPHeaderName(addedHeader)
	route := buildHTTPRoute("ws-backend-filtered", cfg, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix(filteredPath)}},
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
					ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Add: []gatewayv1.HTTPHeader{{Name: headerName, Value: addedValue}},
					},
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef(wsServiceName, wsServicePort, nil)},
		},
	})
	createHTTPRoute(t, k8sClient, route)
	t.Cleanup(func() { deleteAllRoutes(t, k8sClient, cfg) })

	const (
		dialTimeout  = 120 * time.Second
		dialInterval = 3 * time.Second
	)

	deadline := time.Now().Add(dialTimeout)

	var (
		resp        *http.Response
		lastDialErr error
	)

	var attemptConn net.Conn

	for time.Now().Before(deadline) {
		r, c, dialErr := readWSUpgradeResponseThroughTunnel(cfg.TunnelHostname, filteredPath)
		if dialErr != nil {
			lastDialErr = dialErr
			t.Logf("WS upgrade dial through tunnel not ready yet: %v", dialErr)
			time.Sleep(dialInterval)

			continue
		}

		// 101 means the route is in the proxy AND the upgrade
		// succeeded. Anything else (typically 404 during proxy
		// sync) is transient -- retry until the proxy picks up the
		// new route. Closing the intermediate raw conn + response
		// body keeps per-iteration TLS sockets from leaking
		// (bufio.NewReader does not propagate Close to the
		// underlying conn, so resp.Body.Close on its own would not
		// free the FD).
		if r.StatusCode == http.StatusSwitchingProtocols {
			resp = r
			attemptConn = c

			break
		}

		lastDialErr = fmt.Errorf("%w: got status %d", errProxySyncNotConverged, r.StatusCode)
		_ = r.Body.Close()
		_ = c.Close()
		t.Logf("%v", lastDialErr)
		time.Sleep(dialInterval)
	}

	require.NotNil(t, resp,
		"WS upgrade through Cloudflare Tunnel must reach a 101 within %s; last error: %v",
		dialTimeout, lastDialErr)

	t.Cleanup(func() {
		_ = resp.Body.Close()
		_ = attemptConn.Close()
	})

	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode,
		"proxy must propagate the backend's 101 status verbatim through the tunnel")
	assert.Equal(t, addedValue, resp.Header.Get(addedHeader),
		"ResponseHeaderModifier.Add must inject %q into the 101 served through the tunnel — "+
			"the unit / integration tests cover the contract inside the proxy, but only an "+
			"e2e run can confirm cloudflared's HTTP/2 ResponseUserHeaders blob and Cloudflare's "+
			"edge re-serialization preserve the filter-added header end-to-end",
		addedHeader)
}

// readWSUpgradeResponseThroughTunnel performs the WS upgrade handshake
// against the Cloudflare edge by hand: TLS dial, write the upgrade
// request with Host = tunnel hostname, parse the 101 response with
// http.ReadResponse. Returns the raw response AND the underlying conn
// so callers can inspect headers and reliably close the TCP+TLS
// socket -- bufio.NewReader does not propagate Close to the wrapped
// conn, so closing only resp.Body would leak an FD per call. Used by
// TestHTTPRouteBackendProtocolWebSocket_AppliesResponseFilters where
// websocket.Dial would consume the 101 internally and hide it from
// the assertion.
//
// The HTTP/1.1 upgrade request shape mirrors what the existing
// websocket.DialConfig call sends, except the Sec-WebSocket-Key uses
// the RFC 6455 §1.3 example so any reference checks (e.g. expected
// Sec-WebSocket-Accept) are deterministic.
func readWSUpgradeResponseThroughTunnel(tunnelHostname, path string) (*http.Response, net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tlsDialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: tunnelHostname,
		},
	}

	rawConn, err := tlsDialer.DialContext(ctx, "tcp", tunnelHostname+":443")
	if err != nil {
		return nil, nil, fmt.Errorf("tls dial to %s:443: %w", tunnelHostname, err)
	}

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + tunnelHostname + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Origin: https://" + tunnelHostname + "\r\n" +
		"\r\n"

	_, err = rawConn.Write([]byte(req))
	if err != nil {
		_ = rawConn.Close()

		return nil, nil, fmt.Errorf("write upgrade request: %w", err)
	}

	deadlineErr := rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if deadlineErr != nil {
		_ = rawConn.Close()

		return nil, nil, fmt.Errorf("set read deadline: %w", deadlineErr)
	}

	resp, err := http.ReadResponse(bufio.NewReader(rawConn), nil)
	if err != nil {
		_ = rawConn.Close()

		return nil, nil, fmt.Errorf("read 101 response: %w", err)
	}

	return resp, rawConn, nil
}

// setupWSBackendService provisions a Service that exposes a ws-appProtocol
// port on top of an existing app=<selectorApp> deployment. Idempotent: a
// pre-existing service is left untouched.
func setupWSBackendService(
	t *testing.T,
	k8sClient client.Client,
	namespace, serviceName, selectorApp string,
	port int32,
) {
	t.Helper()

	ctx := context.Background()

	existing := &corev1.Service{}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, existing)
	if err == nil {
		t.Logf("service %s/%s already exists, skipping", namespace, serviceName)
		return
	}

	appProto := "kubernetes.io/ws"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": selectorApp},
			Ports: []corev1.ServicePort{
				{
					Name:        "ws",
					Port:        port,
					TargetPort:  intstr.FromInt32(3000),
					Protocol:    corev1.ProtocolTCP,
					AppProtocol: &appProto,
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, svc))
	t.Logf("created ws-appProtocol service %s/%s", namespace, serviceName)
}

// teardownWSBackendService removes the Service created by
// setupWSBackendService. Best-effort: a missing service is not an error.
//
// Honours skipCleanupOnFailure: if the test failed AND the opt-in env var
// is set, returns early without touching the cluster so a maintainer can
// inspect the state at the point of failure. See cleanup_retain_test.go
// for the rationale and the env var name.
func teardownWSBackendService(t *testing.T, k8sClient client.Client, namespace, serviceName string) {
	t.Helper()

	if skipCleanupOnFailure(t) {
		return
	}

	ctx := context.Background()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace}}

	err := k8sClient.Delete(ctx, svc)
	if err != nil {
		t.Logf("teardown: deleting service %s/%s: %v (ignored)", namespace, serviceName, err)
	}
}
