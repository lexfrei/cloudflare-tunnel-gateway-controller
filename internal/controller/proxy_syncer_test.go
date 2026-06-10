package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// testSelfSignedCAPEM emits a fresh PEM-encoded self-signed CA so tests that
// need a non-poisoned trust pool can stand one up without touching the
// filesystem. Used by URI-SAN tests where the resolver must produce a real
// (non-poisoned) BackendTLSConfig.
func testSelfSignedCAPEM(t *testing.T) string {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign,
		IsCA:         true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
}

func TestProxySyncer_SyncRoutes(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	// Set up a mock config API endpoint that records received configs.
	var receivedConfig proxy.Config
	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			err := json.NewDecoder(req.Body).Decode(&receivedConfig)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		"",
		testClient,
		slog.Default(),
	)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							makeBackendRef("web-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	endpoints := []string{configServer.URL + "/config"}

	_, err := syncer.SyncRoutes(context.Background(), endpoints, routes, nil, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.NotEmpty(t, receivedConfig.Rules)
}

func TestProxySyncer_NoRoutes_PushesEmptyConfig(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig)
			if decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		"",
		testClient,
		slog.Default(),
	)

	// Zero routes should still push a valid config with empty rules.
	// The proxy will return 404 for all requests until routes are added.
	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, nil, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.Empty(t, receivedConfig.Rules, "empty routes should produce empty rules")
	assert.True(t, receivedConfig.Version > 0, "version should be positive even with no routes")
}

// TestProxySyncer_ResyncEndpoints_NoLastConfig pins the bootstrap-safe
// no-op: before any SyncRoutes has succeeded the cache is empty, and
// ResyncEndpoints must not invent a config or hit the wire. A new pod
// arriving in this window catches up on the next HTTPRoute reconcile.
func TestProxySyncer_ResyncEndpoints_NoLastConfig(t *testing.T) {
	t.Parallel()

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		"",
		testClient,
		slog.Default(),
	)

	err := syncer.ResyncEndpoints(context.Background(), []string{configServer.URL + "/config"})
	require.NoError(t, err, "ResyncEndpoints must be a no-op before the first SyncRoutes")

	assert.Equal(t, int32(0), pushCount.Load(), "no push must happen when lastCfg is nil")
}

// TestProxySyncer_ResyncEndpoints_ReplaysLastConfig pins issue #293's
// fix: after a successful SyncRoutes, calling ResyncEndpoints with a
// freshly-discovered endpoint pushes the cached config to it without
// touching the HTTPRoute set. The cached version must be preserved
// across the resync (no rebuild, no version bump).
func TestProxySyncer_ResyncEndpoints_ReplaysLastConfig(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var (
		firstReceived  proxy.Config
		secondReceived proxy.Config
		pushCount      atomic.Int32
	)

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			count := pushCount.Add(1)
			target := &firstReceived
			if count == 2 {
				target = &secondReceived
			}

			if decodeErr := json.NewDecoder(req.Body).Decode(target); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	syncer := controller.NewProxySyncer(
		"cluster.local",
		"",
		"",
		testClient,
		slog.Default(),
	)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							makeBackendRef("web-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	endpoint := configServer.URL + "/config"

	_, syncErr := syncer.SyncRoutes(context.Background(), []string{endpoint}, routes, nil, nil, nil)

	require.NoError(t, syncErr)
	require.Equal(t, int32(1), pushCount.Load(), "first sync must push once")
	require.NotEmpty(t, firstReceived.Rules, "first push must carry the built config")

	// Simulate a newly-joined proxy endpoint URL; in production the URL
	// itself is unchanged (the headless Service hostname resolves to a
	// new IP set), but the syncer treats endpoints as opaque strings, so
	// re-pushing to the same URL is a sound proxy for the real scenario.
	require.NoError(t, syncer.ResyncEndpoints(context.Background(), []string{endpoint}))
	require.Equal(t, int32(2), pushCount.Load(), "resync must push once more")

	assert.Equal(t, firstReceived.Version, secondReceived.Version,
		"resync must replay the cached config verbatim -- no version bump, no rebuild")
	assert.Equal(t, len(firstReceived.Rules), len(secondReceived.Rules),
		"resync must replay the cached rules verbatim")
}

func TestProxySyncer_SyncRoutes_H2CBackend(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var receivedConfig proxy.Config

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	h2cProto := "kubernetes.io/h2c"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-svc", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: 8081, AppProtocol: &h2cProto},
				{Name: "http", Port: 80},
			},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("grpc-svc", 8081, 1)},
					},
				},
			},
		},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)

	require.Equal(t, int32(1), pushCount.Load())
	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, receivedConfig.Rules[0].Backends[0].Protocol,
		"backend on a Service port with appProtocol kubernetes.io/h2c must be marked h2c")
	assert.False(t, receivedConfig.HasGRPCRoute,
		"an HTTPRoute with an h2c backend is not a GRPCRoute -- HasGRPCRoute must stay false")
}

// TestProxySyncer_SyncRoutes_GRPCRoute pins the wiring: a
// GRPCRoute handed to SyncRoutes is converted and pushed to the proxy as a
// rule matching the HTTP/2 path /{service}/{method} with an h2c backend,
// merged alongside any HTTP rules.
func TestProxySyncer_SyncRoutes_GRPCRoute(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)

			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	testClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	exact := gatewayv1.GRPCMethodMatchExact
	port := gatewayv1.PortNumber(9000)
	weight := int32(1)
	grpcRoutes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: &exact, Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "echo-svc", Port: &port,
								},
								Weight: &weight,
							}},
						},
					},
				},
			},
		},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, grpcRoutes, nil, nil)
	require.NoError(t, err)

	require.Equal(t, int32(1), pushCount.Load())
	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Matches, 1)
	require.NotNil(t, receivedConfig.Rules[0].Matches[0].Path)
	assert.Equal(t, proxy.PathMatchExact, receivedConfig.Rules[0].Matches[0].Path.Type)
	assert.Equal(t, "/grpc.examples.echo.Echo/UnaryEcho", receivedConfig.Rules[0].Matches[0].Path.Value)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, receivedConfig.Rules[0].Backends[0].Protocol)
	assert.True(t, receivedConfig.HasGRPCRoute,
		"a pushed config carrying a GRPCRoute must set HasGRPCRoute so the proxy can pick http2 at startup")
}

// TestProxySyncer_SyncRoutes_GRPCFailedRefMarked proves the spec-correct 500
// path for a GRPCRoute pointing at a nonexistent Service. The ingress builder
// reports BackendNotFound via GRPCFailedRefs, which SyncRoutes must apply to
// the pushed gRPC rule by marking that backend unavailable (kept in the pool
// with its weight) — otherwise the converter emits a backend with a DNS URL to
// the dead Service and the proxy dials it and returns 502 instead of the
// spec-correct 500 for that fraction. The converter alone never detects a
// missing Service.
func TestProxySyncer_SyncRoutes_GRPCFailedRefMarked(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	testClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	exact := gatewayv1.GRPCMethodMatchExact
	port := gatewayv1.PortNumber(9000)
	grpcRoutes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: &exact, Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "missing-svc", Port: &port,
								},
							}},
						},
					},
				},
			},
		},
	}

	grpcFailedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "echo", BackendName: "missing-svc", BackendNS: "default", Port: 9000, Reason: "BackendNotFound"},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, grpcRoutes, nil, grpcFailedRefs)
	require.NoError(t, err)

	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1, "the invalid gRPC backend stays in the weighted pool")
	assert.Equal(t, http.StatusInternalServerError, receivedConfig.Rules[0].Backends[0].UnavailableStatus,
		"gRPC rule with an invalid backend ref must mark it 500 (not clear it)")
}

// TestProxySyncer_SyncRoutes_InvalidPortBackendMarked500 pins the end-to-end 500
// for an out-of-range port through the full SyncRoutes path. The converter marks
// such a backend 500 inline (the ingress builder's failed-ref for it carries the
// real, invalid port and so host-misses in markUnavailableBackends — harmless,
// because the converter already marked it). This test guards that the backend
// ends up 500 regardless of which path does the marking.
func TestProxySyncer_SyncRoutes_InvalidPortBackendMarked500(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	testClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	badPort := gatewayv1.PortNumber(70000) // out of range
	pathPrefix := gatewayv1.PathMatchPathPrefix
	httpRoutes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-port", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc", Port: &badPort,
								},
							}},
						},
					},
				},
			},
		},
	}

	httpFailedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "bad-port", BackendName: "svc", BackendNS: "default", Port: 70000, Reason: "InvalidPort"},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, httpRoutes, nil, httpFailedRefs, nil)
	require.NoError(t, err)

	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1, "the invalid-port backend stays in the weighted pool")
	assert.Equal(t, http.StatusInternalServerError, receivedConfig.Rules[0].Backends[0].UnavailableStatus,
		"an out-of-range port must mark the backend 500 through the full sync path")
}

// grpcCrossNamespaceBackendSurvives pushes a GRPCRoute whose backend lives in
// another namespace, guarded by a ReferenceGrant whose from.kind is
// grantFromKind, and reports whether the reference was authorized — i.e. the
// pushed rule kept the backend as a VALID, dialable entry. A denied
// cross-namespace ref is invalid, so per the Gateway API spec it now stays in
// the weighted pool marked 500 for its traffic fraction rather than being
// dropped; presence alone therefore no longer implies authorization, an
// unmarked backend does. The proxy is the only v3 data plane, so the validator
// it uses for gRPC must check the grant against from.kind=GRPCRoute, not
// HTTPRoute.
func grpcCrossNamespaceBackendSurvives(t *testing.T, grantFromKind gatewayv1.Kind) bool {
	t.Helper()

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			_ = json.NewDecoder(req.Body).Decode(&receivedConfig)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-grpc", Namespace: "backend-ns"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: grantFromKind, Namespace: "route-ns"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "", Kind: "Service"},
			},
		},
	}
	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	exact := gatewayv1.GRPCMethodMatchExact
	port := gatewayv1.PortNumber(9000)
	backendNS := gatewayv1.Namespace("backend-ns")
	grpcRoutes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "route-ns"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: &exact, Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "echo-svc", Namespace: &backendNS, Port: &port,
								},
							}},
						},
					},
				},
			},
		},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, grpcRoutes, nil, nil)
	require.NoError(t, err)

	require.Len(t, receivedConfig.Rules, 1)

	// Authorized ⇔ a valid (unmarked) backend survives. An invalid ref that the
	// grant did not permit is kept but marked 500, which is NOT survival.
	for _, b := range receivedConfig.Rules[0].Backends {
		if b.UnavailableStatus == 0 {
			return true
		}
	}

	return false
}

// TestProxySyncer_SyncRoutes_GRPCCrossNamespaceAllowedByGRPCGrant: a GRPCRoute
// cross-namespace backend backed by a ReferenceGrant with from.kind=GRPCRoute
// must be allowed by the proxy data plane.
func TestProxySyncer_SyncRoutes_GRPCCrossNamespaceAllowedByGRPCGrant(t *testing.T) {
	t.Parallel()

	assert.True(t, grpcCrossNamespaceBackendSurvives(t, "GRPCRoute"),
		"gRPC cross-namespace backend must survive when a GRPCRoute ReferenceGrant permits it")
}

// TestProxySyncer_SyncRoutes_GRPCCrossNamespaceDeniedByHTTPGrant: an
// HTTPRoute-only ReferenceGrant must NOT authorize a GRPCRoute cross-namespace
// backend — the from.kind must match the actual route kind.
func TestProxySyncer_SyncRoutes_GRPCCrossNamespaceDeniedByHTTPGrant(t *testing.T) {
	t.Parallel()

	assert.False(t, grpcCrossNamespaceBackendSurvives(t, "HTTPRoute"),
		"gRPC cross-namespace backend must be dropped when only an HTTPRoute ReferenceGrant exists")
}

// TestProxySyncer_SyncRoutes_BackendTLSPolicyMissingCA_FailsClosed verifies the
// critical security contract that a BackendTLSPolicy targeting a Service with
// an unresolvable CA bundle must NOT downgrade traffic to plaintext. The
// pushed proxy config MUST include TLS config (with empty CA pool) so the
// handler returns 502 — the operator's stated intent ("must be authenticated
// TLS") is preserved even when enforcement is impossible.
func TestProxySyncer_SyncRoutes_BackendTLSPolicyMissingCA_FailsClosed(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "secure-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Kind: "Service",
						Name: "secure-svc",
					},
				},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "ConfigMap", Name: "does-not-exist"},
				},
				Hostname: "secure.example.com",
			},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "secure-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"secure.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("secure-svc", 8080, 1)},
					},
				},
			},
		},
	}

	_, syncErr := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil, nil, nil)

	require.NoError(t, syncErr)

	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	backend := receivedConfig.Rules[0].Backends[0]

	require.NotNil(t, backend.TLS,
		"BackendTLSPolicy targets the Service but CA can't be resolved — the proxy config MUST carry "+
			"a (poisoned) TLS block so traffic fails closed, NOT nil which would downgrade to plaintext")
	assert.Empty(t, backend.TLS.CABundlePEM,
		"poisoned config has empty CA bundle → handshake fails closed at the proxy")
}

// TestProxySyncer_SyncRoutes_BackendTLSPolicy_URISubjectAltName_PushesURIList
// verifies the URI-SAN happy path end-to-end: when a BackendTLSPolicy carries
// a URI-type SubjectAltName (e.g. SPIFFE ID), the resolver forwards it on
// BackendTLSConfig.SubjectAltNameURIs to the proxy where it's matched against
// the leaf cert's URIs at handshake time. Pairs with
// internal/proxy/integration_test.go's TestHandler_BackendTLSPolicy_URISAN_*
// which exercise the proxy-side matching.
func TestProxySyncer_SyncRoutes_BackendTLSPolicy_URISubjectAltName_PushesURIList(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))

	caCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "spiffe-ca", Namespace: "default"},
		Data:       map[string]string{"ca.crt": testSelfSignedCAPEM(t)},
	}

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "spiffe-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Kind: "Service",
						Name: "spiffe-svc",
					},
				},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "ConfigMap", Name: "spiffe-ca"},
				},
				Hostname: "spiffe.example.com",
				SubjectAltNames: []gatewayv1.SubjectAltName{
					{Type: gatewayv1.URISubjectAltNameType, URI: "spiffe://example.org/server"},
				},
			},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, caCM).Build()

	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "spiffe-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"spiffe.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("spiffe-svc", 8443, 1)},
					},
				},
			},
		},
	}

	_, syncErr := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil, nil, nil)

	require.NoError(t, syncErr)

	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	backend := receivedConfig.Rules[0].Backends[0]

	require.NotNil(t, backend.TLS, "valid policy must produce a real TLS config")
	assert.NotEmpty(t, backend.TLS.CABundlePEM, "valid CA bundle must be forwarded — not poisoned")
	assert.Empty(t, backend.TLS.SubjectAltNames, "no Hostname SAN on the policy → empty DNS list")
	assert.Equal(t, []string{"spiffe://example.org/server"}, backend.TLS.SubjectAltNameURIs,
		"URI SAN must flow through to BackendTLSConfig.SubjectAltNameURIs for proxy-side matching")
}

// Helper functions.

func makeBackendRef(name string, port, weight int) gatewayv1.HTTPBackendRef {
	portNum := gatewayv1.PortNumber(port)
	weightInt := int32(weight)

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &portNum,
			},
			Weight: &weightInt,
		},
	}
}

// syncHeadlessRouteConfig builds a headless Service (clusterIP: None, port 8080 →
// targetPort 3000) with one EndpointSlice whose two endpoints carry the given
// readiness, hands a route referencing it to SyncRoutes, and returns the pushed
// proxy config. Shared by the headless expand / 503 integration tests.
func syncHeadlessRouteConfig(t *testing.T, ready bool) proxy.Config {
	t.Helper()

	var receivedConfig proxy.Config

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if decodeErr := json.NewDecoder(req.Body).Decode(&receivedConfig); decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))

	portName := "first-port"
	endpointPort := int32(3000)
	epReady := ready

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "head", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{Name: portName, Port: 8080, TargetPort: intstr.FromInt32(3000), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "head-ip4",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "head"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &endpointPort}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.1.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &epReady}},
			{Addresses: []string{"10.1.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &epReady}},
		},
	}

	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, slice).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	pathPrefix := gatewayv1.PathMatchPathPrefix
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "head-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"head.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
						BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("head", 8080, 1)},
					},
				},
			},
		},
	}

	_, err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)

	return receivedConfig
}

// TestProxySyncer_SyncRoutes_HeadlessBackend pins the end-to-end wiring: a route
// to a headless Service with ready endpoints is pushed to the proxy as one
// backend per endpoint, dialing the targetPort (3000) — not the Service port.
func TestProxySyncer_SyncRoutes_HeadlessBackend(t *testing.T) {
	t.Parallel()

	cfg := syncHeadlessRouteConfig(t, true)

	require.Len(t, cfg.Rules, 1)

	urls := make([]string, 0, len(cfg.Rules[0].Backends))
	for i := range cfg.Rules[0].Backends {
		urls = append(urls, cfg.Rules[0].Backends[i].URL)
		assert.Zero(t, cfg.Rules[0].Backends[i].UnavailableStatus, "ready endpoints are dialable, not marked")
	}

	assert.ElementsMatch(t, []string{"http://10.1.0.1:3000", "http://10.1.0.2:3000"}, urls,
		"a headless Service expands to one backend per ready endpoint at the targetPort")
}

// TestProxySyncer_SyncRoutes_HeadlessNoReadyEndpoints503 pins the ordering: a
// headless Service with no ready endpoints keeps its FQDN backend and is marked
// 503 by the zero-endpoint pass (not silently dropped, not expanded).
func TestProxySyncer_SyncRoutes_HeadlessNoReadyEndpoints503(t *testing.T) {
	t.Parallel()

	cfg := syncHeadlessRouteConfig(t, false)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, "http://head.default.svc.cluster.local:8080", cfg.Rules[0].Backends[0].URL,
		"no ready endpoints → the FQDN backend is kept")
	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"and is marked 503 by the zero-endpoint pass")
}

// TestProxySyncer_SkipsPushWhenConfigUnchanged pins the incremental-sync
// contract on the proxy side: a sync whose built config is identical to the
// last successfully pushed one (same routes, same endpoints) must not push
// again -- steady-state reconciles (status updates, endpoint heartbeats)
// otherwise re-push the full config on every event. A route change must
// push, and after it the unchanged config is skipped again.
func TestProxySyncer_SkipsPushWhenConfigUnchanged(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var pushCount atomic.Int32

	configServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCount.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer configServer.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	makeRoutes := func(path string) []*gatewayv1.HTTPRoute {
		return []*gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: &path}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("web-svc", 80, 1)},
				}},
			},
		}}
	}

	endpoints := []string{configServer.URL + "/config"}

	_, err := syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/"), nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int32(1), pushCount.Load(), "first sync must push")

	_, err = syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/"), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(1), pushCount.Load(), "identical config to identical endpoints must not push again")

	_, err = syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/changed"), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), pushCount.Load(), "a route change must push")

	_, err = syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/changed"), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), pushCount.Load(), "steady state after the change must be skipped again")
}

// TestProxySyncer_EndpointSetChangePushesUnchangedConfig pins that the skip
// is keyed on the endpoint set too: the same config must still be delivered
// when the proxy replica set changes (a new pod has never seen it).
func TestProxySyncer_EndpointSetChangePushesUnchangedConfig(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var pushCountA, pushCountB atomic.Int32

	serverA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCountA.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCountB.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	path := "/"
	routes := []*gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{
					{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: &path}},
				},
				BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("web-svc", 80, 1)},
			}},
		},
	}}

	_, err := syncer.SyncRoutes(context.Background(), []string{serverA.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int32(1), pushCountA.Load())

	// Same config, but a second replica joins: both must receive the push.
	_, err = syncer.SyncRoutes(context.Background(),
		[]string{serverA.URL + "/config", serverB.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), pushCountA.Load(), "existing replica gets the re-push when the set changes")
	assert.Equal(t, int32(1), pushCountB.Load(), "new replica must receive the config")
}

// TestProxySyncer_PartialPushFailureInvalidatesSkip pins the recovery
// contract of the steady-state skip: after a push that partially succeeded
// (one replica took the new config, another errored), a subsequent sync back
// to the previous config MUST push -- the skip key must be invalidated on
// failure, or a rollback after a partial push leaves the half-updated
// replica stale forever.
func TestProxySyncer_PartialPushFailureInvalidatesSkip(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	var pushCountA, pushCountB atomic.Int32

	var failB atomic.Bool

	serverA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushCountA.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			if failB.Load() {
				writer.WriteHeader(http.StatusInternalServerError)

				return
			}

			pushCountB.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	makeRoutes := func(path string) []*gatewayv1.HTTPRoute {
		return []*gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: &path}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{makeBackendRef("web-svc", 80, 1)},
				}},
			},
		}}
	}

	endpoints := []string{serverA.URL + "/config", serverB.URL + "/config"}

	// Config A reaches both replicas.
	_, err := syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/a"), nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int32(1), pushCountA.Load())
	require.Equal(t, int32(1), pushCountB.Load())

	// Config B partially succeeds: replica A takes it, replica B errors.
	failB.Store(true)

	_, err = syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/b"), nil, nil, nil)
	require.Error(t, err, "a partial push must surface as an error")
	require.Equal(t, int32(2), pushCountA.Load(), "replica A accepted config B")

	// Roll back to config A: replica A still holds B, so the sync MUST push
	// even though config A matches the last fully-successful push.
	failB.Store(false)

	_, err = syncer.SyncRoutes(context.Background(), endpoints, makeRoutes("/a"), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(3), pushCountA.Load(),
		"the rollback after a partial push must be delivered, not skipped")
	assert.Equal(t, int32(2), pushCountB.Load())
}
