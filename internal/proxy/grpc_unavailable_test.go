package proxy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const grpcUnavailableHost = "grpc.example.com"

// unservableGRPCConfigs covers every handler path that refuses to dial a
// backend: the rule-level fail-closed mark, a marked backendRef, no backend
// to select, an unparsable backend URL and a leftover ExternalBackend
// sentinel. GRPCRoute requires each to reach a gRPC client as UNAVAILABLE.
func unservableGRPCConfigs() map[string]proxy.RouteRule {
	const live = "http://backend.default.svc.cluster.local:8080"

	return map[string]proxy.RouteRule{
		"rule unavailable": {
			UnavailableStatus: http.StatusInternalServerError,
			Backends:          []proxy.BackendRef{{URL: live, Weight: 1}},
		},
		"backend unavailable": {
			Backends: []proxy.BackendRef{{URL: live, Weight: 1, UnavailableStatus: http.StatusInternalServerError}},
		},
		"no backends":       {},
		"zero weight":       {Backends: []proxy.BackendRef{{URL: live, Weight: 0}}},
		"unparsable URL":    {Backends: []proxy.BackendRef{{URL: "http://%zz", Weight: 1}}},
		"external sentinel": {Backends: []proxy.BackendRef{{URL: "externalbackend://default/api", Weight: 1}}},
	}
}

func newUnservableGRPCHandler(t *testing.T, rule proxy.RouteRule) *proxy.Handler {
	t.Helper()

	rule.Hostnames = []string{grpcUnavailableHost}
	rule.Matches = []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}}

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{Version: 1, Rules: []proxy.RouteRule{rule}}))

	return proxy.NewHandler(router)
}

// TestGRPCClientObservesUnavailableForUnservableBackend drives a real grpc-go
// client over the standalone HTTP/2 server.
func TestGRPCClientObservesUnavailableForUnservableBackend(t *testing.T) {
	t.Parallel()

	for name, rule := range unservableGRPCConfigs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewUnstartedServer(newUnservableGRPCHandler(t, rule))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)

			pool := x509.NewCertPool()
			pool.AddCert(srv.Certificate())

			conn, err := grpc.NewClient(
				"passthrough:///"+srv.Listener.Addr().String(),
				grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})),
				grpc.WithAuthority(grpcUnavailableHost),
				grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
			)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			var out []byte

			err = conn.Invoke(ctx, "/pkg.Service/Method", []byte{}, &out)
			require.Equalf(t, codes.Unavailable, status.Code(err), "got: %v", err)
		})
	}
}

// TestUnservableBackendGRPCStatus_TunnelWriter runs the same paths through the
// cloudflared HTTP/2 writer fake: the status must ride a grpc-status trailer
// on an HTTP 200, the only shape the tunnel's trailer bridge forwards.
func TestUnservableBackendGRPCStatus_TunnelWriter(t *testing.T) {
	t.Parallel()

	for name, rule := range unservableGRPCConfigs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"http://"+grpcUnavailableHost+"/pkg.Service/Method", nil)
			req.Header.Set("Content-Type", "application/grpc")

			fake := newFakeCloudflaredRespWriter()
			newUnservableGRPCHandler(t, rule).ServeHTTP(fake, req)

			assert.Equal(t, http.StatusOK, fake.Status())
			assert.Equal(t, "14", fake.Header().Get(http.TrailerPrefix+"Grpc-Status"))
		})
	}
}

// TestUnservableBackendPlainHTTPStays500 keeps the HTTPRoute contract: a
// non-gRPC request on the same paths still gets HTTP 500.
func TestUnservableBackendPlainHTTPStays500(t *testing.T) {
	t.Parallel()

	for name, rule := range unservableGRPCConfigs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+grpcUnavailableHost+"/", nil)
			rec := httptest.NewRecorder()
			newUnservableGRPCHandler(t, rule).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Empty(t, rec.Header().Get(http.TrailerPrefix+"Grpc-Status"))
		})
	}
}
