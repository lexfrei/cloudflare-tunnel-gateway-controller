//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
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
// proxy matches the route, sees a Connection: Upgrade header pair, and lets
// `httputil.ReverseProxy.handleUpgradeResponse` hijack the conn on the 101
// response. Bytes then flow bidirectionally between the test client and the
// echo-basic backend's `/ws` endpoint.
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
