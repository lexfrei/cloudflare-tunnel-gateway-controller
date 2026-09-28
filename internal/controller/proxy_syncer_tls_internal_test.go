package controller

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const planeServerName = "cf-proxy-edge-config.tenant-a.svc.cluster.local"

func servingPlane(t *testing.T, issuer *configtls.Authority, servedName string) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	certPEM, keyPEM, err := issuer.Issue([]string{servedName}, time.Now())
	require.NoError(t, err)

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	var puts atomic.Int32

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			puts.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)

	return server, &puts
}

// TestPush_VerifiesTheConfiguredHostNotThePodIP pins the name the handshake is
// checked against: the host of the configured endpoint, which the resolved
// URL no longer carries. A plane is reached at a pod IP; verifying that IP
// instead would accept any leaf naming it and reject the plane's real one.
func TestPush_VerifiesTheConfiguredHostNotThePodIP(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	syncer := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(),
		slog.New(slog.DiscardHandler), WithConfigAPIAuthority(authority))

	named, namedPuts := servingPlane(t, authority, planeServerName)
	results := syncer.push(context.Background(), &proxy.Config{Version: 1},
		[]pushEndpoint{{url: named.URL + "/config", serverName: planeServerName}}, "token")
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.Equal(t, int32(1), namedPuts.Load())

	byIP, byIPPuts := servingPlane(t, authority, "127.0.0.1")
	results = syncer.push(context.Background(), &proxy.Config{Version: 1},
		[]pushEndpoint{{url: byIP.URL + "/config", serverName: planeServerName}}, "token")
	require.Len(t, results, 1)
	require.Error(t, results[0].Err, "a leaf naming only the pod IP must not answer for the plane")
	assert.Equal(t, int32(0), byIPPuts.Load())
}

// TestPerGatewayConfigEndpoint_FollowsTLS pins the URL a per-Gateway plane is
// pushed to: https exactly when the syncer holds a CA, since an http URL
// under TLS is refused and one under plaintext would fail the handshake.
func TestPerGatewayConfigEndpoint_FollowsTLS(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "tenant-a"}}

	plain := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(), slog.New(slog.DiscardHandler))
	assert.Equal(t, "http://cf-proxy-edge-config.tenant-a.svc.cluster.local:8081/config",
		plain.perGatewayConfigEndpoint(gateway, "cluster.local", 0))

	withTLS := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(), slog.New(slog.DiscardHandler),
		WithConfigAPIAuthority(testAuthority(t)))
	assert.Equal(t, "https://cf-proxy-edge-config.tenant-a.svc.cluster.local:8081/config",
		withTLS.perGatewayConfigEndpoint(gateway, "cluster.local", 0))
}

// TestResolveEndpoints_CarriesTheConfiguredHost pins that resolution records
// the configured host beside every URL it rewrites or keeps.
func TestResolveEndpoints_CarriesTheConfiguredHost(t *testing.T) {
	t.Parallel()

	resolved := resolveEndpoints(context.Background(), net.DefaultResolver.LookupHost, []string{
		"https://127.0.0.1:8081/config",
		"https://unresolvable.invalid:8081/config",
	})

	require.Len(t, resolved, 2)
	assert.Equal(t, "127.0.0.1", resolved[0].serverName)
	assert.Equal(t, pushEndpoint{url: "https://unresolvable.invalid:8081/config", serverName: "unresolvable.invalid"}, resolved[1])
}
