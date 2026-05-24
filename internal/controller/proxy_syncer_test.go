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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
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

	err := syncer.SyncRoutes(context.Background(), endpoints, routes, nil)
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
	err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pushCount.Load())
	assert.Empty(t, receivedConfig.Rules, "empty routes should produce empty rules")
	assert.True(t, receivedConfig.Version > 0, "version should be positive even with no routes")
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

	err := syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil)
	require.NoError(t, err)

	require.Equal(t, int32(1), pushCount.Load())
	require.Len(t, receivedConfig.Rules, 1)
	require.Len(t, receivedConfig.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, receivedConfig.Rules[0].Backends[0].Protocol,
		"backend on a Service port with appProtocol kubernetes.io/h2c must be marked h2c")
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

	require.NoError(t, syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil))

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

	require.NoError(t, syncer.SyncRoutes(context.Background(), []string{configServer.URL + "/config"}, routes, nil))

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
