package proxy_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/websocket"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// newBackend creates an httptest.Server that sets X-Backend to the given name
// and echoes the received path in X-Received-Path. The server is automatically
// closed when the test completes.
func newBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("X-Backend", name)
		writer.Header().Set("X-Received-Path", req.URL.Path)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server
}

// newEchoHeadersBackend creates a backend that echoes received request headers
// back as response headers with an "X-Echo-" prefix. The server is
// automatically closed when the test completes.
func newEchoHeadersBackend(t *testing.T, headerNames ...string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		for _, name := range headerNames {
			if val := req.Header.Get(name); val != "" {
				writer.Header().Set("X-Echo-"+name, val)
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server
}

// newH2CBackend creates an httptest.Server that serves HTTP/2 cleartext (h2c)
// via the stdlib's native UnencryptedHTTP2 protocol (Go 1.24+). It reports the
// protocol it saw the request on via X-Backend-Proto so tests can assert the
// proxy actually negotiated h2c. The server is closed on cleanup.
func newH2CBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()

	backend := http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("X-Backend", name)
		writer.Header().Set("X-Backend-Proto", req.Proto)
		writer.WriteHeader(http.StatusOK)
	})

	var protocols http.Protocols

	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	server := httptest.NewUnstartedServer(backend)
	server.Config.Protocols = &protocols
	server.Start()
	t.Cleanup(server.Close)

	return server
}

func TestHandler_BackendProtocolH2C(t *testing.T) {
	t.Parallel()

	backend := newH2CBackend(t, "h2c")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
				},
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolH2C},
				},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "h2c", resp.Header.Get("X-Backend"))
	assert.Equal(t, "HTTP/2.0", resp.Header.Get("X-Backend-Proto"),
		"proxy must speak HTTP/2 cleartext (h2c) to a backend with appProtocol kubernetes.io/h2c")
}

func TestNewTransport_H2C_HasLivenessDefaults(t *testing.T) {
	t.Parallel()

	rt := proxy.NewTransportForTest(proxy.BackendProtocolH2C, nil)

	tr, ok := rt.(*http2.Transport)
	require.True(t, ok, "h2c transport must be *http2.Transport, got %T", rt)
	assert.NotZero(t, tr.ReadIdleTimeout,
		"h2c transport must set ReadIdleTimeout so dead TCP connections get evicted from the multiplexed pool")
	assert.NotZero(t, tr.PingTimeout,
		"h2c transport must set PingTimeout to bound how long a stuck PING blocks request progress")
}

func TestNewH2CDialer_HasTimeouts(t *testing.T) {
	t.Parallel()

	dialer := proxy.NewH2CDialerForTest()

	// Without a Timeout the dialer would wait for kernel TCP defaults (often
	// >1 minute) on a hung SYN, stalling the proxy request well past any
	// sensible request budget. KeepAlive evicts half-closed connections from
	// the pool. Both must mirror http.DefaultTransport's dialer config.
	assert.NotZero(t, dialer.Timeout, "h2c dialer must set Timeout to bound TCP SYN waits")
	assert.NotZero(t, dialer.KeepAlive, "h2c dialer must set KeepAlive to evict half-closed pool connections")
}

// countingMirrorBackend records the number of received requests on an atomic
// counter and 200s every one. Used to assert mirror filter behavior.
func countingMirrorBackend(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, &hits
}

func TestHandler_MultipleMirrors_AllBackendsHit(t *testing.T) {
	t.Parallel()

	primary := newBackend(t, "primary")
	mirrorA, hitsA := countingMirrorBackend(t)
	mirrorB, hitsB := countingMirrorBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorA.URL}},
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorB.URL}},
				},
				Backends: []proxy.BackendRef{{URL: primary.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Result().StatusCode)

	// Mirror requests are fire-and-forget goroutines; give them a moment.
	require.Eventually(t, func() bool {
		return hitsA.Load() == 1 && hitsB.Load() == 1
	}, 3*time.Second, 20*time.Millisecond,
		"both mirror backends must receive exactly one mirrored request (got A=%d B=%d)", hitsA.Load(), hitsB.Load())
}

// bodyEchoBackend captures the full request body of each request so tests can
// assert mirror filters didn't truncate or share the body reader.
func bodyEchoBackend(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()

	var last atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		last.Store(string(body))
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, &last
}

func TestHandler_MultipleMirrors_PostBody_BothReceiveFullBody(t *testing.T) {
	t.Parallel()

	primary := newBackend(t, "primary")
	mirrorA, lastA := bodyEchoBackend(t)
	mirrorB, lastB := bodyEchoBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorA.URL}},
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorB.URL}},
				},
				Backends: []proxy.BackendRef{{URL: primary.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	const payload = "this body must reach every mirror identically"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Result().StatusCode)

	require.Eventually(t, func() bool {
		a, _ := lastA.Load().(string)
		b, _ := lastB.Load().(string)

		return a == payload && b == payload
	}, 3*time.Second, 20*time.Millisecond,
		"both mirror backends must receive the full unmodified body via independent readers")
}

func TestHandler_MultipleMirrors_OversizeBody_PrimaryStillSucceeds(t *testing.T) {
	t.Parallel()

	// 1 MiB + 1 byte: deliberately just past maxMirrorBodySize (1 << 20) so
	// every mirror's bufferMirrorBody hits the oversize branch.
	const oversize = (1 << 20) + 1

	primary, primaryLast := bodyEchoBackend(t)
	mirrorA, hitsA := countingMirrorBackend(t)
	mirrorB, hitsB := countingMirrorBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorA.URL}},
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorB.URL}},
				},
				Backends: []proxy.BackendRef{{URL: primary.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	body := strings.Repeat("a", oversize)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Result().StatusCode,
		"primary must still succeed even when both mirrors skip on oversize body")

	// Give any spurious mirror goroutines a chance to run before asserting zero.
	time.Sleep(200 * time.Millisecond)

	assert.Zero(t, hitsA.Load(), "first mirror must skip on oversize body, got %d hits", hitsA.Load())
	assert.Zero(t, hitsB.Load(), "second mirror must skip on oversize body, got %d hits", hitsB.Load())

	got, _ := primaryLast.Load().(string)
	assert.Len(t, got, oversize, "primary must receive the full body despite mirror skips")
}

func TestHandler_MirrorPercentZero_NeverMirrors(t *testing.T) {
	t.Parallel()

	primary := newBackend(t, "primary")
	mirror, hits := countingMirrorBackend(t)
	zero := int32(0)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirror.URL, Percent: &zero}},
				},
				Backends: []proxy.BackendRef{{URL: primary.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	for range 100 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Allow goroutines a moment in case any leaked through.
	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, hits.Load(), "Percent=0 must never mirror; got %d requests", hits.Load())
}

func TestHandler_MirrorPercentDistribution(t *testing.T) {
	t.Parallel()

	primary := newBackend(t, "primary")
	mirror, hits := countingMirrorBackend(t)
	twenty := int32(20)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: mirror.URL, Percent: &twenty}},
				},
				Backends: []proxy.BackendRef{{URL: primary.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	const totalRequests = 500
	for range totalRequests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Wait for fire-and-forget goroutines to drain. Require at least one
	// hit before declaring "stable", otherwise the initial poll fires before
	// any mirror goroutine has had a chance to run (hits==0, sleep, hits==0,
	// "stable") and the assertion below evaluates a falsely-empty counter.
	require.Eventually(t, func() bool {
		first := hits.Load()
		if first == 0 {
			return false
		}

		time.Sleep(100 * time.Millisecond)

		return hits.Load() == first
	}, 10*time.Second, 200*time.Millisecond)

	got := int(hits.Load())
	// Upstream conformance uses ±15% tolerance over 500 requests; mirror that.
	// 20% expected → ~100 mirrored; allow [50, 150].
	const expected = totalRequests * 20 / 100
	const tolerance = totalRequests * 15 / 100
	assert.GreaterOrEqual(t, got, expected-tolerance,
		"mirrored request count %d below 20%% expected ±15%% tolerance", got)
	assert.LessOrEqual(t, got, expected+tolerance,
		"mirrored request count %d above 20%% expected ±15%% tolerance", got)
}

// caPEMFromTLSServer returns the PEM-encoded test certificate authority that
// httptest.NewTLSServer self-signed for itself. Suitable as a CABundle for
// BackendTLSConfig so the proxy trusts the test server.
func caPEMFromTLSServer(t *testing.T, server *httptest.Server) string {
	t.Helper()

	require.NotNil(t, server.TLS, "expected *httptest.Server in TLS mode")
	require.NotEmpty(t, server.TLS.Certificates)

	cert := server.TLS.Certificates[0].Certificate[0]
	pemBlock := &pem.Block{Type: "CERTIFICATE", Bytes: cert}

	return string(pem.EncodeToMemory(pemBlock))
}

// uriSANTestServer is the shared output of newURITLSServer: a TLS-enabled
// httptest.Server plus the PEM-encoded CA bundle (== the self-signed leaf,
// since it's its own root) that BackendTLSConfig should use as the trust
// anchor.
type uriSANTestServer struct {
	Server *httptest.Server
	CAPEM  string
}

// newURITLSServer spins up a TLS httptest.Server whose certificate carries
// the supplied DNS SANs AND URI SANs. Used to exercise the URI-SAN matching
// path that the Gateway API BackendTLSPolicySANValidation conformance test
// drives. The cert is self-signed; CAPEM is the same cert encoded as PEM so
// the proxy trusts it.
func newURITLSServer(t *testing.T, dnsSANs []string, uriSANs []string) *uriSANTestServer {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	uris := make([]*url.URL, 0, len(uriSANs))
	for _, raw := range uriSANs {
		u, parseErr := url.Parse(raw)
		require.NoError(t, parseErr, "failed to parse test URI SAN %q", raw)
		uris = append(uris, u)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "uri-san-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              append([]string{}, dnsSANs...),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		URIs:                  uris,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "uri-san-tls")
		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{tlsCert},
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	return &uriSANTestServer{Server: server, CAPEM: string(certPEM)}
}

func TestHandler_BackendTLSPolicy_ValidCA_Succeeds(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "tls")
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							ServerName:  "example.com", // httptest.NewTLSServer cert SAN
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "tls", resp.Header.Get("X-Backend"))
}

func TestHandler_BackendTLSPolicy_BogusCA_Fails(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	bogusCA := `-----BEGIN CERTIFICATE-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1234567890BOGUSCAFORTEST
-----END CERTIFICATE-----
`

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: bogusCA,
							ServerName:  "example.com",
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"BackendTLSPolicy with mismatched CA must fail TLS verification → proxy returns 502")
}

func TestHandler_BackendTLSPolicy_MismatchedHostname_Fails(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							ServerName:  "wrong-hostname.invalid",
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"BackendTLSPolicy with hostname not in cert SAN must fail TLS verification → proxy returns 502")
}

// TestHandler_BackendTLSPolicy_PoisonedConfig_HandshakeFails verifies the
// end-to-end fail-closed contract: when the controller pushes a config with an
// empty CABundlePEM (e.g. because the BackendTLSPolicy CA reference cannot be
// resolved), the proxy MUST refuse the request via a 502 rather than silently
// downgrading to plaintext. Pairs with
// TestProxySyncer_SyncRoutes_BackendTLSPolicyMissingCA_FailsClosed which
// covers the config-push side; this test pins the runtime side.
func TestHandler_BackendTLSPolicy_PoisonedConfig_HandshakeFails(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						// Poisoned config: empty CA bundle. The controller
						// pushes exactly this when a policy targets the
						// Service but its CA ref cannot be resolved.
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: "",
							ServerName:  "example.com",
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"poisoned TLS config (empty CA pool) MUST fail TLS verification at handshake → 502, "+
			"the operator's intent is preserved over silent plaintext downgrade")
}

// TestHandler_BackendTLSPolicy_SANOnly_AcceptsMatchingSAN verifies the
// SAN-list authentication path: when SubjectAltNames is non-empty the proxy
// uses x509.VerifyHostname against each entry rather than treating ServerName
// as an authentication identity. The first SAN in the list does NOT match the
// cert, the second one DOES — OR-semantics must accept.
func TestHandler_BackendTLSPolicy_SANOnly_AcceptsMatchingSAN(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							// ServerName intentionally set to a value the cert
							// does NOT carry — when SANs are present, this
							// must be used for SNI only and the leaf
							// authentication runs over the SAN list below.
							ServerName: "sni-only.invalid",
							SubjectAltNames: []string{
								"does-not-match.invalid",
								"example.com", // matches httptest.NewTLSServer cert SAN
							},
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Result().StatusCode,
		"SAN OR-matching must succeed when any policy SAN matches the cert")
}

// TestHandler_BackendTLSPolicy_SANOnly_RejectsAllMismatching verifies that the
// SAN-list authentication path returns 502 when NO policy SAN matches the
// cert. ServerName must NOT rescue the request — it's SNI only in this mode.
func TestHandler_BackendTLSPolicy_SANOnly_RejectsAllMismatching(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							// ServerName matches the cert — but since SANs are
							// set, ServerName must NOT be used for auth.
							ServerName: "example.com",
							SubjectAltNames: []string{
								"only-wrong.invalid",
								"also-wrong.invalid",
							},
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"SAN OR-matching must reject when NO policy SAN matches, regardless of ServerName")
}

// TestHandler_BackendTLSPolicy_URISAN_AcceptsMatchingURI verifies that a
// policy with URI-type SubjectAltName (e.g. SPIFFE ID) succeeds when the
// backend cert presents an exact-matching URI SAN. The Gateway API
// BackendTLSPolicySANValidation conformance test drives this end-to-end.
func TestHandler_BackendTLSPolicy_URISAN_AcceptsMatchingURI(t *testing.T) {
	t.Parallel()

	srv := newURITLSServer(t,
		[]string{"abc.example.com"},
		[]string{"spiffe://abc.example.com/test-identity"},
	)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    srv.Server.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: srv.CAPEM,
							// ServerName must NOT be used for auth (per spec) when SANs are set.
							// Use a value not in the cert to prove that.
							ServerName:         "sni-only.invalid",
							SubjectAltNameURIs: []string{"spiffe://abc.example.com/test-identity"},
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Result().StatusCode,
		"URI SAN matching the cert's URI must accept the handshake")
}

// TestHandler_BackendTLSPolicy_URISAN_RejectsMismatching verifies that a
// policy with URI SAN that doesn't appear in the leaf cert fails the
// handshake — the proxy returns 502.
func TestHandler_BackendTLSPolicy_URISAN_RejectsMismatching(t *testing.T) {
	t.Parallel()

	srv := newURITLSServer(t,
		[]string{"abc.example.com"},
		[]string{"spiffe://abc.example.com/test-identity"},
	)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    srv.Server.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM:        srv.CAPEM,
							ServerName:         "abc.example.com",
							SubjectAltNameURIs: []string{"spiffe://def.example.com/test-identity"},
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"URI SAN not present in the cert must fail TLS verification → proxy returns 502")
}

// TestHandler_BackendTLSPolicy_MixedSAN_OrMatch verifies that when a policy
// lists BOTH DNS hostnames AND URI SANs, the matching is OR — either path
// passing accepts the handshake. Matches the Gateway API multiple-sans test.
func TestHandler_BackendTLSPolicy_MixedSAN_OrMatch(t *testing.T) {
	t.Parallel()

	srv := newURITLSServer(t,
		[]string{"abc.example.com"},
		[]string{"spiffe://abc.example.com/test-identity"},
	)

	cases := []struct {
		name string
		dns  []string
		uris []string
	}{
		{
			name: "dns matches, uri does not",
			dns:  []string{"abc.example.com"},
			uris: []string{"spiffe://def.example.com/test-identity"},
		},
		{
			name: "uri matches, dns does not",
			dns:  []string{"def.example.com"},
			uris: []string{"spiffe://abc.example.com/test-identity"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			router := proxy.NewRouter()
			require.NoError(t, router.UpdateConfig(&proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Hostnames: []string{"app.example.com"},
						Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
						Backends: []proxy.BackendRef{
							{
								URL:    srv.Server.URL,
								Weight: 1,
								TLS: &proxy.BackendTLSConfig{
									CABundlePEM:        srv.CAPEM,
									ServerName:         "sni-only.invalid",
									SubjectAltNames:    tc.dns,
									SubjectAltNameURIs: tc.uris,
								},
							},
						},
					},
				},
			}))

			handler := proxy.NewHandler(router)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Result().StatusCode,
				"OR semantics: either DNS SAN or URI SAN matching must accept the handshake")
		})
	}
}

// TestHandler_BackendTLSPolicy_MixedSAN_AllMismatch confirms that when neither
// DNS nor URI lists match the cert, the handshake fails closed.
func TestHandler_BackendTLSPolicy_MixedSAN_AllMismatch(t *testing.T) {
	t.Parallel()

	srv := newURITLSServer(t,
		[]string{"abc.example.com"},
		[]string{"spiffe://abc.example.com/test-identity"},
	)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    srv.Server.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM:        srv.CAPEM,
							ServerName:         "abc.example.com",
							SubjectAltNames:    []string{"def.example.com"},
							SubjectAltNameURIs: []string{"spiffe://def.example.com/test-identity"},
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"with neither DNS nor URI SAN matching, both lists must fail → proxy returns 502")
}

// TestHandler_CORSFilter_PreflightShortCircuits drives a CORS preflight
// through the full handler stack: filter pipeline → 204 + headers, backend
// never touched (verified via a request counter).
func TestHandler_CORSFilter_PreflightShortCircuits(t *testing.T) {
	t.Parallel()

	backend, hits := countingMirrorBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterCORS,
						CORS: &proxy.CORSConfig{
							AllowOrigins:     []string{"https://www.foo.com"},
							AllowMethods:     []string{"GET", "OPTIONS"},
							AllowHeaders:     []string{"x-header-1"},
							AllowCredentials: true,
							MaxAge:           3600,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "http://app.example.com/x", nil)
	req.Header.Set("Origin", "https://www.foo.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "x-header-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode,
		"preflight MUST short-circuit at the filter with a 204")
	assert.Equal(t, "https://www.foo.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, OPTIONS", resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "3600", resp.Header.Get("Access-Control-Max-Age"))
	assert.Equal(t, int32(0), hits.Load(),
		"backend MUST NOT be hit by a CORS preflight — the filter short-circuits before proxying")
}

// TestHandler_CORSFilter_SimpleRequestPassesThroughAndStampsHeaders verifies
// the simple-request path: a GET with matched Origin reaches the backend AND
// the response carries CORS headers on the way back.
func TestHandler_CORSFilter_SimpleRequestPassesThroughAndStampsHeaders(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "primary")

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterCORS,
						CORS: &proxy.CORSConfig{
							AllowOrigins:     []string{"https://www.foo.com"},
							AllowCredentials: true,
							ExposeHeaders:    []string{"x-extra"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	req.Header.Set("Origin", "https://www.foo.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "primary", resp.Header.Get("X-Backend"),
		"simple request MUST reach the backend; CORS only stamps response headers")
	assert.Equal(t, "https://www.foo.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "x-extra", resp.Header.Get("Access-Control-Expose-Headers"))
}

// TestHandler_CORSFilter_NonMatchedOriginNoHeaders confirms a cross-origin
// simple request from a non-allowed Origin reaches the backend but the
// response carries NO CORS headers — the browser then fails the read.
func TestHandler_CORSFilter_NonMatchedOriginNoHeaders(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "primary")

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterCORS,
						CORS: &proxy.CORSConfig{AllowOrigins: []string{"https://www.foo.com"}},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	req.Header.Set("Origin", "https://attacker.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"non-allowed Origin MUST NOT receive Access-Control-Allow-Origin")
}

func TestHandler_BackendProtocolToggleSameHostPort(t *testing.T) {
	t.Parallel()

	// One backend server serving BOTH HTTP/1.1 and h2c on the same host:port —
	// http.Protocols accepts both. This pins the scenario where the only thing
	// that changes between configs is the Service appProtocol, not the address.
	backend := newH2CBackend(t, "dualproto")

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP}},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	// Phase 1: HTTP/1.1 path primes the per-host transport cache.
	req1 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	resp1 := rec1.Result()
	defer resp1.Body.Close()

	require.Equal(t, http.StatusOK, resp1.StatusCode)
	require.Equal(t, "HTTP/1.1", resp1.Header.Get("X-Backend-Proto"),
		"phase 1 must speak HTTP/1.1 to prime the wrong transport")

	// Phase 2: same backend.URL, same host:port — only Protocol flipped to h2c.
	// A cache keyed on host alone would reuse the HTTP/1.1 transport here.
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 2,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolH2C}},
			},
		},
	}))

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	resp2 := rec2.Result()
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, "HTTP/2.0", resp2.Header.Get("X-Backend-Proto"),
		"after the Protocol flip to h2c on the same host:port, the proxy must NOT reuse the cached HTTP/1.1 transport")
}

func TestHandler_PathMatchPrecedence(t *testing.T) {
	t.Parallel()

	exactBackend := newBackend(t, "exact")
	prefixBackend := newBackend(t, "prefix")
	regexBackend := newBackend(t, "regex")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/v1/users"}},
				},
				Backends: []proxy.BackendRef{{URL: exactBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
				},
				Backends: []proxy.BackendRef{{URL: prefixBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "/api/v[0-9]+/.*"}},
				},
				Backends: []proxy.BackendRef{{URL: regexBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		path            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "exact wins over prefix and regex",
			path:            "/api/v1/users",
			expectedBackend: "exact",
			expectedStatus:  http.StatusOK,
		},
		{
			// Per Gateway API spec, regex paths have higher precedence than
			// prefix paths, so the regex rule wins.
			name:            "regex wins over prefix per gateway api precedence",
			path:            "/api/v1/orders",
			expectedBackend: "regex",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "only prefix matches",
			path:            "/api/health",
			expectedBackend: "prefix",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "no match returns 404",
			path:           "/other",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_HeaderBasedRouting(t *testing.T) {
	t.Parallel()

	prodBackend := newBackend(t, "prod")
	stagingBackend := newBackend(t, "staging")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "prod"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: prodBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "staging"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: stagingBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		headerValue     string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "prod header routes to prod",
			headerValue:     "prod",
			expectedBackend: "prod",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "staging header routes to staging",
			headerValue:     "staging",
			expectedBackend: "staging",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "no matching header returns 404",
			headerValue:    "dev",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
			req.Header.Set("X-Env", tt.headerValue)

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_QueryParamRouting(t *testing.T) {
	t.Parallel()

	jsonBackend := newBackend(t, "json")
	xmlBackend := newBackend(t, "xml")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "json"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: jsonBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "xml"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: xmlBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		query           string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "json query param routes to json backend",
			query:           "?format=json",
			expectedBackend: "json",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "xml query param routes to xml backend",
			query:           "?format=xml",
			expectedBackend: "xml",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "unknown format returns 404",
			query:          "?format=csv",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/"+tt.query, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_MethodBasedRouting(t *testing.T) {
	t.Parallel()

	readerBackend := newBackend(t, "reader")
	writerBackend := newBackend(t, "writer")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/data"},
						Method: http.MethodGet,
					},
				},
				Backends: []proxy.BackendRef{{URL: readerBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/data"},
						Method: http.MethodPost,
					},
				},
				Backends: []proxy.BackendRef{{URL: writerBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		method          string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "GET routes to reader",
			method:          http.MethodGet,
			expectedBackend: "reader",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "POST routes to writer",
			method:          http.MethodPost,
			expectedBackend: "writer",
			expectedStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://app.example.com/api/data", nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)
			assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
		})
	}
}

func TestHandler_CombinedMatchConditions(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "combined")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"},
						Method: http.MethodGet,
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Version", Value: "v2"},
						},
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "json"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name           string
		method         string
		path           string
		headers        map[string]string
		query          string
		expectedStatus int
	}{
		{
			name:           "all conditions match",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "wrong method",
			method:         http.MethodPost,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong path",
			method:         http.MethodGet,
			path:           "/other",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong header",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v1"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "missing header",
			method:         http.MethodGet,
			path:           "/api/data",
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong query param",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=xml",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "missing query param",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://app.example.com"+tt.path+tt.query, nil)

			for key, val := range tt.headers {
				req.Header.Set(key, val)
			}

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)
		})
	}
}

func TestHandler_MultipleMatchesOR(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "matched")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/health"}},
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/ready"}},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		path            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "first match block",
			path:            "/health",
			expectedBackend: "matched",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "second match block",
			path:            "/ready",
			expectedBackend: "matched",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "neither match returns 404",
			path:           "/other",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_URLRewriteFullPath(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "rewrite")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/old"}},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Path: &proxy.URLRewritePath{
								Type:            proxy.URLRewriteFullPath,
								ReplaceFullPath: new("/new/path"),
							},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/old/foo", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "/new/path", recorder.Header().Get("X-Received-Path"))
}

func TestHandler_URLRewritePrefixMatch(t *testing.T) {
	t.Parallel()

	// The handler now automatically sets the matched prefix from the RouteResult,
	// so no wrapper is needed. The prefix is stored in the request context
	// before filters are applied.

	backend := newBackend(t, "prefix-rewrite")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v1"}},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Path: &proxy.URLRewritePath{
								Type:               proxy.URLRewritePrefixMatch,
								ReplacePrefixMatch: new("/v2"),
							},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/v1/users/123", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "/v2/users/123", recorder.Header().Get("X-Received-Path"))
}

func TestHandler_RedirectSchemeAndHost(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"old.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestRedirect,
						RequestRedirect: &proxy.RedirectConfig{
							Scheme:     new("https"),
							Hostname:   new("new.example.com"),
							StatusCode: new(http.StatusMovedPermanently),
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://unused:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://old.example.com/path", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusMovedPermanently, recorder.Code)
	assert.Equal(t, "https://new.example.com/path", recorder.Header().Get("Location"))
}

func TestHandler_WildcardHostnameRouting(t *testing.T) {
	t.Parallel()

	wildcardBackend := newBackend(t, "wildcard")
	exactBackend := newBackend(t, "exact")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"},
				Backends:  []proxy.BackendRef{{URL: wildcardBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"api.example.com"},
				Backends:  []proxy.BackendRef{{URL: exactBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		host            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "exact host wins over wildcard",
			host:            "api.example.com",
			expectedBackend: "exact",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "wildcard matches other subdomains",
			host:            "web.example.com",
			expectedBackend: "wildcard",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "different domain returns 404",
			host:           "other.com",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+tt.host+"/", nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_WeightedBackendsDistribution(t *testing.T) {
	t.Parallel()

	backendA := newBackend(t, "A")
	backendB := newBackend(t, "B")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends: []proxy.BackendRef{
					{URL: backendA.URL, Weight: 80},
					{URL: backendB.URL, Weight: 20},
				},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	const totalRequests = 1000

	counts := map[string]int{}

	for range totalRequests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, req)

		backend := recorder.Header().Get("X-Backend")
		counts[backend]++
	}

	ratioA := float64(counts["A"]) / float64(totalRequests)
	ratioB := float64(counts["B"]) / float64(totalRequests)

	assert.InDelta(t, 0.80, ratioA, 0.15, "backend A should receive ~80%% of traffic, got %.2f%%", ratioA*100)
	assert.InDelta(t, 0.20, ratioB, 0.15, "backend B should receive ~20%% of traffic, got %.2f%%", ratioB*100)

	// Sanity check: both backends received at least some traffic.
	assert.Greater(t, counts["A"], 0, "backend A should receive at least one request")
	assert.Greater(t, counts["B"], 0, "backend B should receive at least one request")
}

func TestHandler_RequestMirrorDoesNotAffectResponse(t *testing.T) {
	t.Parallel()

	primaryBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "primary")
		writer.WriteHeader(http.StatusOK)

		_, err := writer.Write([]byte("primary response"))
		if err != nil {
			t.Errorf("failed to write primary response: %v", err)
		}
	}))
	t.Cleanup(primaryBackend.Close)

	var mirrorReceived atomic.Bool

	mirrorBackend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Simulate a slow mirror backend.
		time.Sleep(50 * time.Millisecond)
		mirrorReceived.Store(true)
	}))
	t.Cleanup(mirrorBackend.Close)

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type:          proxy.FilterRequestMirror,
						RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorBackend.URL},
					},
				},
				Backends: []proxy.BackendRef{{URL: primaryBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	// Client gets the primary response immediately.
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "primary", recorder.Header().Get("X-Backend"))

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	assert.Equal(t, "primary response", string(body))

	// Wait for mirror to eventually receive the request.
	assert.Eventually(t, func() bool {
		return mirrorReceived.Load()
	}, 2*time.Second, 10*time.Millisecond, "mirror backend should eventually receive the request")
}

func TestHandler_MultipleFiltersApplied(t *testing.T) {
	t.Parallel()

	backend := newEchoHeadersBackend(t, "X-A", "X-B", "X-Internal")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestHeaderModifier,
						RequestHeaderModifier: &proxy.HeaderModifier{
							Set:    []proxy.HeaderValue{{Name: "X-A", Value: "1"}},
							Add:    []proxy.HeaderValue{{Name: "X-B", Value: "2"}},
							Remove: []string{"X-Internal"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	req.Header.Set("X-Internal", "secret-value")

	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	// X-A was set by the filter.
	assert.Equal(t, "1", recorder.Header().Get("X-Echo-X-A"))

	// X-B was added by the filter.
	assert.Equal(t, "2", recorder.Header().Get("X-Echo-X-B"))

	// X-Internal was removed by the filter, so the backend should not echo it.
	assert.Empty(t, recorder.Header().Get("X-Echo-X-Internal"))
}

// TestHandler_BackendClientCert_PresentedAtHandshake exercises the end-to-end
// path the GatewayBackendClientCertificateFeature conformance test verifies:
// the proxy presents the configured client certificate during the backend
// TLS handshake, and the backend can observe it via TLS.PeerCertificates.
func TestHandler_BackendClientCert_PresentedAtHandshake(t *testing.T) {
	t.Parallel()

	clientCertPEM, clientKeyPEM := generateClientKeypairPEM(t, "gateway-mtls-tenant")

	// Backend server: TLS with RequireAnyClientCert so the handshake succeeds
	// for any presented cert; the handler records what came through.
	var receivedCN atomic.Value

	tlsBackend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
			receivedCN.Store(req.TLS.PeerCertificates[0].Subject.CommonName)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	tlsBackend.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAnyClientCert,
	}
	tlsBackend.StartTLS()

	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM:   caPEM,
							ServerName:    "example.com",
							ClientCertPEM: clientCertPEM,
							ClientKeyPEM:  clientKeyPEM,
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "gateway-mtls-tenant", receivedCN.Load(),
		"backend must observe the client cert the proxy presented in the TLS handshake")
}

// TestHandler_BackendClientCert_MissingWhenServerRequires verifies the
// fail-closed path: a server that requires a client cert and the proxy that
// does NOT configure one must produce a 502 (handshake failure), not a 200.
func TestHandler_BackendClientCert_MissingWhenServerRequires(t *testing.T) {
	t.Parallel()

	tlsBackend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	tlsBackend.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAnyClientCert,
	}
	tlsBackend.StartTLS()

	t.Cleanup(tlsBackend.Close)

	caPEM := caPEMFromTLSServer(t, tlsBackend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:    tlsBackend.URL,
						Weight: 1,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							ServerName:  "example.com",
							// No ClientCertPEM/ClientKeyPEM: the backend will reject the handshake.
						},
					},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Result().StatusCode,
		"backend that requires a client cert must reject a handshake without one — proxy returns 502")
}

// newWSEchoBackend stands up an httptest backend running an x/net/websocket
// echo handler. tlsEnabled chooses between httptest.NewServer and
// NewTLSServer; the returned *httptest.Server carries the TLS material that
// caPEMFromTLSServer reads.
//
// The WS handler echoes raw bytes back; tests write a frame and expect to
// read the same bytes. golang.org/x/net/websocket frames text payloads with
// FrameType 1 automatically.
func newWSEchoBackend(t *testing.T, tlsEnabled bool) *httptest.Server {
	t.Helper()

	handler := websocket.Server{
		Handler: func(c *websocket.Conn) {
			// io.Copy reads frames from the client and writes them back
			// verbatim. It returns when the client closes.
			_, _ = io.Copy(c, c)
		},
	}

	if tlsEnabled {
		server := httptest.NewTLSServer(handler)
		t.Cleanup(server.Close)

		return server
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

// TestHandler_BackendProtocolWebSocket_Echo pins the contract that
// httputil.ReverseProxy's native 101 Switching Protocols handling survives
// our Director. The Director currently only deletes X-Original-Host and
// sets a default User-Agent — Connection/Upgrade/Sec-WebSocket-* flow
// through. If anyone adds header sanitisation here in the future, this
// test fails loudly: WebSocket support is silently broken without a real
// upgrade handshake to prove otherwise.
//
// The proxy itself runs as a real httptest.Server because httptest
// .NewRecorder does not implement http.Hijacker, which ReverseProxy needs
// in handleUpgradeResponse to hijack the conn after the 101.
func TestHandler_BackendProtocolWebSocket_Echo(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, false)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				// Empty Hostnames → match-all. Avoids forcing the test
				// client to forge a Host header on top of the dialed
				// loopback address.
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	proxySrv := httptest.NewServer(proxy.NewHandler(router))
	t.Cleanup(proxySrv.Close)

	roundTripWSEcho(t, proxySrv, "ws-cleartext-round-trip")
}

// TestHandler_BackendProtocolWebSocket_TLS_Echo is the wss/BackendTLSPolicy
// variant: the backend speaks WS-over-TLS and the proxy must complete the
// TLS handshake before the WebSocket upgrade. Pins that the cert flows
// through cleanly and the 101 still hijacks.
func TestHandler_BackendProtocolWebSocket_TLS_Echo(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, true)
	caPEM := caPEMFromTLSServer(t, backend)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{
						URL:       backend.URL, // already https://
						Weight:    1,
						Protocol:  proxy.BackendProtocolHTTP,
						WebSocket: true,
						TLS: &proxy.BackendTLSConfig{
							CABundlePEM: caPEM,
							ServerName:  "example.com", // httptest.NewTLSServer cert SAN
						},
					},
				},
			},
		},
	}))

	proxySrv := httptest.NewServer(proxy.NewHandler(router))
	t.Cleanup(proxySrv.Close)

	roundTripWSEcho(t, proxySrv, "wss-tls-round-trip")
}

// TestHandler_WebSocket_NotTerminatedByRequestTimeout pins that a route's
// per-rule `timeouts.request` does NOT kill an active WebSocket once the
// 101 Switching Protocols response has hijacked the conn. The Gateway API
// spec defines `timeouts.request` as a bound on how long the gateway takes
// to respond to an HTTP request; once the protocol switches via upgrade,
// the HTTP request is complete and the post-upgrade bytestream is not an
// HTTP request anymore. Applying an HTTP request timeout to a long-lived
// WS conn would be a footgun — operators would set a 30s timeout for
// regular traffic and discover their WS clients silently dropped.
//
// Without the upgrade-skip fix in Handler.ServeHTTP, stdlib's
// httputil.ReverseProxy.handleUpgradeResponse watches req.Context() and
// closes both ends of the hijacked conn when ctx is canceled — so a
// request timeout of 1s would terminate the WS at the 1s mark regardless
// of whether bytes were still flowing.
func TestHandler_WebSocket_NotTerminatedByRequestTimeout(t *testing.T) {
	t.Parallel()
	runWebSocketTimeoutSkipTest(t, &proxy.RouteTimeouts{Request: 500 * time.Millisecond})
}

// TestHandler_WebSocket_NotTerminatedByBackendTimeout is the sibling pin
// for `timeouts.backend`. Both timeouts get the same upgrade-skip
// treatment in Handler.proxyToBackend, but without an explicit test a
// future change that drops the `!isHTTPUpgradeRequest(req)` guard on the
// backend-timeout arm would sail through CI because the request-timeout
// test on its own would still pass.
func TestHandler_WebSocket_NotTerminatedByBackendTimeout(t *testing.T) {
	t.Parallel()
	runWebSocketTimeoutSkipTest(t, &proxy.RouteTimeouts{Backend: 500 * time.Millisecond})
}

// runWebSocketTimeoutSkipTest sets up a WS echo backend, applies the given
// per-rule timeouts to the route, opens a WebSocket, sleeps past the
// configured deadline, and confirms the round trip still works. The
// behaviour is identical for `Request` and `Backend` timeouts because
// both arms of the handler now skip context.WithTimeout for HTTP/1.1
// upgrade requests.
func runWebSocketTimeoutSkipTest(t *testing.T, timeouts *proxy.RouteTimeouts) {
	t.Helper()

	backend := newWSEchoBackend(t, false)

	router := proxy.NewRouter()

	// Apply a very tight 500ms timeout so the test takes < 2s but is
	// large enough to comfortably absorb the upgrade handshake.
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Timeouts: timeouts,
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true}},
			},
		},
	}))

	proxySrv := httptest.NewServer(proxy.NewHandler(router))
	t.Cleanup(proxySrv.Close)

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/echo"
	origin := "http://" + proxySrv.Listener.Addr().String()

	conn, err := websocket.Dial(wsURL, "", origin)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	// Sleep past the timeout — without the upgrade-skip fix this would
	// have canceled the request context and closed the hijacked conn.
	time.Sleep(1 * time.Second)

	const payload = "after-timeout-payload"

	_, err = conn.Write([]byte(payload))
	require.NoError(t, err,
		"writing a frame >1s after the 500ms timeout must succeed — "+
			"the upgrade response detached the conn from the request lifecycle")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, len(payload))
	n, err := io.ReadFull(conn, buf)
	require.NoError(t, err, "reading the echoed frame back must succeed after the timeout")
	assert.Equal(t, payload, string(buf[:n]))
}

// TestHandler_NonWSBackend_TimeoutAppliesEvenWithUpgradeHeaders is the
// regression guard for the upgrade-skip's operator-driven gate. A request
// arrives at a route whose backends have NO `appProtocol: kubernetes.io/ws`
// declaration but carries the WebSocket upgrade headers anyway (a
// misconfigured or malicious client). The route's Request timeout MUST
// still apply — otherwise any client could bypass operator-declared
// deadlines by tacking on `Connection: Upgrade, Upgrade: websocket`, a
// slow-loris vector and a Gateway API spec violation on non-WS routes.
//
// The backend here is a slow-write HTTP/1.1 server that takes 2s to write
// its response body; with a 500ms request timeout the proxy must return
// 504 Gateway Timeout, NOT proxy through.
func TestHandler_NonWSBackend_TimeoutAppliesEvenWithUpgradeHeaders(t *testing.T) {
	t.Parallel()

	// Slow backend: hold the response for longer than the request timeout
	// so the timeout decides the outcome rather than the backend latency.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Timeouts: &proxy.RouteTimeouts{Request: 500 * time.Millisecond},
				Backends: []proxy.BackendRef{
					// NB: WebSocket is intentionally false — operator did NOT
					// declare this backend as WS-capable. The timeout-skip
					// must NOT trigger even with client-supplied upgrade
					// headers.
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	start := time.Now()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusGatewayTimeout, rec.Result().StatusCode,
		"client claiming upgrade against a non-WebSocket backend MUST still hit the 500ms request timeout — "+
			"gating on the client header alone would let any request bypass operator-declared deadlines")
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"the handler must return inside the timeout window, not block for the full 2s backend latency")
}

// roundTripWSEcho dials the proxy via ws://, writes a payload, and reads
// it back. Extracted so the cleartext and TLS variants share the assertion
// shape — the only difference between them is the backend configuration.
func roundTripWSEcho(t *testing.T, proxySrv *httptest.Server, payload string) {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/echo"
	origin := "http://" + proxySrv.Listener.Addr().String()

	conn, err := websocket.Dial(wsURL, "", origin)
	require.NoError(t, err, "WebSocket handshake through proxy must succeed (101 Switching Protocols)")

	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Write([]byte(payload))
	require.NoError(t, err, "writing frame after 101 must succeed (conn is hijacked, bytes go to backend)")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, len(payload))
	n, err := io.ReadFull(conn, buf)
	require.NoError(t, err, "reading the echoed frame back must succeed")
	assert.Equal(t, payload, string(buf[:n]),
		"echo round-trip through proxy must preserve payload verbatim — "+
			"any difference proves the Director / transport corrupted the upgrade")
}
