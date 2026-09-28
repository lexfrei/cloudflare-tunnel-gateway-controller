package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
)

const tlsTestServerName = "proxy-headless.test.svc.cluster.local"

// writeTLSPair issues a leaf for tlsTestServerName and writes it to dir,
// returning the files and the authority that signed it.
func writeTLSPair(t *testing.T, dir string) (string, string, *configtls.Authority) {
	t.Helper()

	caCert, caKey, err := configtls.NewAuthorityPEM(time.Now())
	require.NoError(t, err)

	authority, err := configtls.LoadAuthority(caCert, caKey)
	require.NoError(t, err)

	certPEM, keyPEM, err := authority.Issue([]string{tlsTestServerName}, time.Now())
	require.NoError(t, err)

	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	return certFile, keyFile, authority
}

func unsetTLSEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{configTLSCertFileEnv, configTLSKeyFileEnv} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
}

// TestConfigAPITLSFromEnv_UnsetServesPlaintext pins the standalone/dev path
// and the operator opt-out: no files, no TLS.
func TestConfigAPITLSFromEnv_UnsetServesPlaintext(t *testing.T) {
	unsetTLSEnv(t)

	cfg, err := configAPITLSFromEnv(slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

// TestConfigAPITLSFromEnv_HalfPairIsFatal pins that one file without the other
// is a broken mount, never a quiet fallback to plaintext.
func TestConfigAPITLSFromEnv_HalfPairIsFatal(t *testing.T) {
	for _, name := range []string{configTLSCertFileEnv, configTLSKeyFileEnv} {
		t.Run(name, func(t *testing.T) {
			unsetTLSEnv(t)
			t.Setenv(name, "/some/file")

			_, err := configAPITLSFromEnv(slog.New(slog.DiscardHandler))
			require.ErrorIs(t, err, errConfigTLSHalfPair)
		})
	}
}

// TestConfigAPITLSFromEnv_UnreadablePairIsFatal pins that a pair that cannot
// be loaded stops startup rather than serving plaintext.
func TestConfigAPITLSFromEnv_UnreadablePairIsFatal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configTLSCertFileEnv, filepath.Join(dir, "missing.crt"))
	t.Setenv(configTLSKeyFileEnv, filepath.Join(dir, "missing.key"))

	_, err := configAPITLSFromEnv(slog.New(slog.DiscardHandler))
	require.Error(t, err)
}

// TestRun_RefusesABrokenTLSConfiguration pins the call site in run.
func TestRun_RefusesABrokenTLSConfiguration(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "")
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("PROXY_AUTH_TOKEN", "")
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	t.Setenv("PROXY_TRACING_ENABLED", "")
	unsetTLSEnv(t)
	t.Setenv(configTLSCertFileEnv, "/some/file")

	var logs bytes.Buffer

	assert.Equal(t, 1, run(slog.New(slog.NewTextHandler(&logs, nil))))
	assert.Contains(t, logs.String(), "refusing to start with a broken config-API TLS configuration")
}

func healthzRequest(t *testing.T, target string) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	require.NoError(t, err)

	return req
}

func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// TestServeConfigAPI_ServesTLSWhenConfigured pins that the config API listener
// speaks TLS with the mounted leaf, and that a plaintext client gets nothing
// the handler served.
func TestServeConfigAPI_ServesTLSWhenConfigured(t *testing.T) {
	unsetTLSEnv(t)

	certFile, keyFile, authority := writeTLSPair(t, t.TempDir())
	t.Setenv(configTLSCertFileEnv, certFile)
	t.Setenv(configTLSKeyFileEnv, keyFile)

	tlsConfig, err := configAPITLSFromEnv(slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)

	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})

	server := newServer(addr, handler)
	server.TLSConfig = tlsConfig

	failed := serveConfigAPI(slog.New(slog.DiscardHandler), server, cancel)

	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	tlsClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: authority.ClientConfig(tlsTestServerName)},
	}

	require.Eventually(t, func() bool {
		resp, getErr := tlsClient.Do(healthzRequest(t, "https://"+addr+"/healthz"))
		if getErr != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusNoContent
	}, 5*time.Second, 50*time.Millisecond)

	plainClient := &http.Client{Timeout: 5 * time.Second}

	resp, err := plainClient.Do(healthzRequest(t, "http://"+addr+"/healthz"))
	if err == nil {
		_ = resp.Body.Close()
		assert.NotEqual(t, http.StatusNoContent, resp.StatusCode, "plaintext must not reach the handler")
	}

	assert.False(t, failed.Load())
	assert.NoError(t, ctx.Err(), "a serving TLS listener must not cancel the tunnel")
}

// TestServeConfigAPI_TLSServerConfigIsTLS13 pins the protocol floor.
func TestServeConfigAPI_TLSServerConfigIsTLS13(t *testing.T) {
	unsetTLSEnv(t)

	certFile, keyFile, _ := writeTLSPair(t, t.TempDir())
	t.Setenv(configTLSCertFileEnv, certFile)
	t.Setenv(configTLSKeyFileEnv, keyFile)

	tlsConfig, err := configAPITLSFromEnv(slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), tlsConfig.MinVersion)
}
