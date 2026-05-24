package proxy_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// generateClientKeypairPEM produces a self-signed leaf certificate and matching
// private key as PEM byte slices. The returned cert carries CommonName=cn so
// tests can assert on the parsed Leaf subject. The order of returned values is
// (cert, key) — fail loudly at compile time rather than via a named-return
// convention.
func generateClientKeypairPEM(t *testing.T, cn string) ([]byte, []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

func TestBuildBackendTLSConfig_ClientCert_Attached(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypairPEM(t, "gateway-mtls-leaf")

	cfg := proxy.BuildBackendTLSConfigForTest(
		&proxy.BackendTLSConfig{
			CABundlePEM:   "",
			ServerName:    "backend.example.com",
			ClientCertPEM: certPEM,
			ClientKeyPEM:  keyPEM,
		},
		x509.NewCertPool(),
	)

	require.NotNil(t, cfg)
	require.Len(t, cfg.Certificates, 1, "client cert keypair must be attached")
	require.NotNil(t, cfg.Certificates[0].Leaf, "stdlib should parse leaf eagerly via X509KeyPair")
	assert.Equal(t, "gateway-mtls-leaf", cfg.Certificates[0].Leaf.Subject.CommonName)
}

func TestBuildBackendTLSConfig_ClientCert_InvalidPEM_LeavesEmpty(t *testing.T) {
	t.Parallel()

	cfg := proxy.BuildBackendTLSConfigForTest(
		&proxy.BackendTLSConfig{
			CABundlePEM:   "",
			ServerName:    "backend.example.com",
			ClientCertPEM: []byte("not a real PEM"),
			ClientKeyPEM:  []byte("not a real PEM"),
		},
		x509.NewCertPool(),
	)

	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Certificates, "malformed keypair must fail closed — no half-attached cert")
}

func TestBuildBackendTLSConfig_NoClientCert_NoCertificates(t *testing.T) {
	t.Parallel()

	cfg := proxy.BuildBackendTLSConfigForTest(
		&proxy.BackendTLSConfig{
			CABundlePEM: "",
			ServerName:  "backend.example.com",
		},
		x509.NewCertPool(),
	)

	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Certificates)
}

func TestTransportKey_DistinguishesByClientCert(t *testing.T) {
	t.Parallel()

	certA, keyA := generateClientKeypairPEM(t, "tenant-a")
	certB, keyB := generateClientKeypairPEM(t, "tenant-b")

	host := "backend.example.com:8443"
	protocol := proxy.BackendProtocolHTTP

	noCert := &proxy.BackendTLSConfig{ServerName: "backend.example.com"}
	certAOnly := &proxy.BackendTLSConfig{
		ServerName:    "backend.example.com",
		ClientCertPEM: certA,
		ClientKeyPEM:  keyA,
	}
	certBOnly := &proxy.BackendTLSConfig{
		ServerName:    "backend.example.com",
		ClientCertPEM: certB,
		ClientKeyPEM:  keyB,
	}

	keyNoCert := proxy.TransportKey(host, protocol, noCert)
	keyCertA := proxy.TransportKey(host, protocol, certAOnly)
	keyCertB := proxy.TransportKey(host, protocol, certBOnly)

	// Two Gateways serving the same backend host with distinct client certs
	// must not share a cached transport — otherwise a connection from one
	// Gateway would present the other's cert.
	assert.NotEqual(t, keyNoCert, keyCertA, "presence of client cert must change pool key")
	assert.NotEqual(t, keyCertA, keyCertB, "different client cert must change pool key")
	assert.NotEqual(t, keyNoCert, keyCertB)

	// Determinism: same config produces same key.
	assert.Equal(t, keyCertA, proxy.TransportKey(host, protocol, certAOnly))
}

func TestBuildBackendTLSConfig_ClientCert_WithSANConstraints(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypairPEM(t, "gateway-mtls-san-mode")

	// The SAN-list path (mode 2) takes a different branch of buildBackendTLSConfig.
	// Confirm the client cert is attached in that mode too — both verification
	// modes must support backend mTLS identically.
	cfg := proxy.BuildBackendTLSConfigForTest(
		&proxy.BackendTLSConfig{
			CABundlePEM:     "",
			ServerName:      "backend.example.com",
			SubjectAltNames: []string{"backend.example.com"},
			ClientCertPEM:   certPEM,
			ClientKeyPEM:    keyPEM,
		},
		x509.NewCertPool(),
	)

	require.NotNil(t, cfg)
	require.Len(t, cfg.Certificates, 1)
	assert.Equal(t, "gateway-mtls-san-mode", cfg.Certificates[0].Leaf.Subject.CommonName)
}
