package controller_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
)

func newTestAuthority(t *testing.T) *configtls.Authority {
	t.Helper()

	certPEM, keyPEM, err := configtls.NewAuthorityPEM(time.Now())
	require.NoError(t, err)

	authority, err := configtls.LoadAuthority(certPEM, keyPEM)
	require.NoError(t, err)

	return authority
}

// tlsConfigServer is a config API that serves a leaf issued by issuer for
// servedName and counts the PUTs that reach it.
type tlsConfigServer struct {
	server *httptest.Server
	puts   atomic.Int32
}

func newTLSConfigServer(t *testing.T, issuer *configtls.Authority, servedName string) *tlsConfigServer {
	t.Helper()

	certPEM, keyPEM, err := issuer.Issue([]string{servedName}, time.Now())
	require.NoError(t, err)

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	loader, err := configtls.NewCertificateLoader(certFile, keyFile, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	recorder := &tlsConfigServer{}
	recorder.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			recorder.puts.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	recorder.server.TLS = loader.ServerConfig()
	recorder.server.StartTLS()
	t.Cleanup(recorder.server.Close)

	return recorder
}

func tlsTestRoutes() []*gatewayv1.HTTPRoute {
	return []*gatewayv1.HTTPRoute{partitionTestRoute("r", "tls.example.com", "svc")}
}

func newTLSSyncer(authority *configtls.Authority) *controller.ProxySyncer {
	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	return controller.NewProxySyncer("cluster.local", "token", "", testClient, slog.New(slog.DiscardHandler),
		controller.WithConfigAPIAuthority(authority))
}

// TestProxySyncer_TLS_PushesToAPlaneServingOurLeaf pins the happy path: a
// plane serving a leaf from the controller's CA for the endpoint's own name
// receives the push.
func TestProxySyncer_TLS_PushesToAPlaneServingOurLeaf(t *testing.T) {
	t.Parallel()

	authority := newTestAuthority(t)
	plane := newTLSConfigServer(t, authority, "127.0.0.1")

	_, err := newTLSSyncer(authority).SyncPartition(context.Background(), 0, "default/gw", "token",
		[]string{plane.server.URL + "/config"}, tlsTestRoutes(), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(1), plane.puts.Load())
}

// TestProxySyncer_TLS_RefusesALeafFromAnotherCA pins the CA pin: a plane
// presenting a certificate the controller did not issue — a tenant's own
// Secret in its namespace, say — never receives the config or the token.
func TestProxySyncer_TLS_RefusesALeafFromAnotherCA(t *testing.T) {
	t.Parallel()

	authority := newTestAuthority(t)
	plane := newTLSConfigServer(t, newTestAuthority(t), "127.0.0.1")

	_, err := newTLSSyncer(authority).SyncPartition(context.Background(), 0, "default/gw", "token",
		[]string{plane.server.URL + "/config"}, tlsTestRoutes(), nil, nil, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), plane.puts.Load())
}

// TestProxySyncer_TLS_RefusesALeafIssuedForAnotherPlane pins the name check:
// a genuine leaf from our CA that names a different plane cannot receive this
// plane's config.
func TestProxySyncer_TLS_RefusesALeafIssuedForAnotherPlane(t *testing.T) {
	t.Parallel()

	authority := newTestAuthority(t)
	plane := newTLSConfigServer(t, authority, "192.0.2.10")

	_, err := newTLSSyncer(authority).SyncPartition(context.Background(), 0, "default/gw", "token",
		[]string{plane.server.URL + "/config"}, tlsTestRoutes(), nil, nil, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), plane.puts.Load())
}

// TestProxySyncer_TLS_RefusesAPlaintextEndpoint pins that TLS mode never
// sends the config over plaintext, even when handed an http:// URL.
func TestProxySyncer_TLS_RefusesAPlaintextEndpoint(t *testing.T) {
	t.Parallel()

	plain := newRecordingConfigServer(t)

	_, err := newTLSSyncer(newTestAuthority(t)).SyncPartition(context.Background(), 0, "default/gw", "token",
		[]string{plain.server.URL + "/config"}, tlsTestRoutes(), nil, nil, nil)
	require.ErrorIs(t, err, controller.ErrPlaintextConfigEndpoint)
	assert.Equal(t, 0, plain.pushCount())
}

// TestProxySyncer_TLS_ResyncUsesTheSamePin pins the replay path, which pushes
// on its own and must not bypass the verification.
func TestProxySyncer_TLS_ResyncUsesTheSamePin(t *testing.T) {
	t.Parallel()

	authority := newTestAuthority(t)
	good := newTLSConfigServer(t, authority, "127.0.0.1")
	syncer := newTLSSyncer(authority)

	_, err := syncer.SyncRoutes(context.Background(), 0, []string{good.server.URL + "/config"},
		tlsTestRoutes(), nil, nil, nil)
	require.NoError(t, err)

	rogue := newTLSConfigServer(t, newTestAuthority(t), "127.0.0.1")

	require.Error(t, syncer.ResyncEndpoints(context.Background(), []string{rogue.server.URL + "/config"}))
	assert.Equal(t, int32(0), rogue.puts.Load())
}
