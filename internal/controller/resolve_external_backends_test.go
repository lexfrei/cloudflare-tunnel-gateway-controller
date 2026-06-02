package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func ebScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return scheme
}

func TestResolveExternalBackends_RewritesSentinel(t *testing.T) {
	t.Parallel()

	eb := &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-api", Namespace: "default"},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendSchemeHTTPS, Host: "api.example.com", Port: 8443, Path: "/v1",
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).WithObjects(eb).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: proxy.ExternalBackendSentinelURL("default", "ext-api"), Weight: 1},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, "https://api.example.com:8443/v1", cfg.Rules[0].Backends[0].URL,
		"sentinel must be rewritten to the ExternalBackend's real URL")
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus)
}

func TestResolveExternalBackends_MissingMarks500(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: proxy.ExternalBackendSentinelURL("default", "gone"), Weight: 1},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a missing ExternalBackend must be marked 500 for its traffic fraction")
}

func TestResolveExternalBackends_MalformedMarks500(t *testing.T) {
	t.Parallel()

	// host:port in the host field slips past the CRD pattern; the resolved URL
	// would fail url.Parse. Mark 500 without pushing the bad URL to the proxy.
	eb := &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-bad", Namespace: "default"},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendSchemeHTTPS, Host: "internal-api:8080", Port: 443,
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).WithObjects(eb).Build()

	sentinel := proxy.ExternalBackendSentinelURL("default", "ext-bad")
	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: sentinel, Weight: 1},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a malformed ExternalBackend must be marked 500, not pushed as a bad URL")
	assert.Equal(t, sentinel, cfg.Rules[0].Backends[0].URL,
		"the malformed URL must not be written into the pushed config")
}

func TestResolveExternalBackends_ClearsH2COnHTTPS(t *testing.T) {
	t.Parallel()

	eb := &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-grpc", Namespace: "default"},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendSchemeHTTPS, Host: "grpc.example.com", Port: 443,
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).WithObjects(eb).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: proxy.ExternalBackendSentinelURL("default", "ext-grpc"), Weight: 1, Protocol: proxy.BackendProtocolH2C},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, "https://grpc.example.com:443", cfg.Rules[0].Backends[0].URL)
	assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol,
		"h2c over an https origin must be cleared so the TLS transport negotiates HTTP/2 via ALPN")
}

func TestResolveExternalBackends_KeepsH2COnHTTP(t *testing.T) {
	t.Parallel()

	eb := &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-grpc", Namespace: "default"},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendSchemeHTTP, Host: "grpc.internal", Port: 8080,
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).WithObjects(eb).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: proxy.ExternalBackendSentinelURL("default", "ext-grpc"), Weight: 1, Protocol: proxy.BackendProtocolH2C},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, "http://grpc.internal:8080", cfg.Rules[0].Backends[0].URL)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol,
		"h2c over an http origin (cleartext gRPC) must be preserved")
}

func TestResolveExternalBackends_IgnoresNonSentinel(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(ebScheme(t)).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://svc.default.svc.cluster.local:80", Weight: 1},
	}}}}

	resolveExternalBackends(context.Background(), cli, cfg)

	assert.Equal(t, "http://svc.default.svc.cluster.local:80", cfg.Rules[0].Backends[0].URL,
		"a normal Service URL must be left untouched")
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus)
}
