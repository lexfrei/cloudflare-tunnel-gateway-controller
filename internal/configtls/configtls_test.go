package configtls_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
)

const planeName = "cf-proxy-gw-config.tenant-a.svc.cluster.local"

//nolint:gochecknoglobals // fixed clock shared by every case
var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newAuthority(t *testing.T) *configtls.Authority {
	t.Helper()

	certPEM, keyPEM, err := configtls.NewAuthorityPEM(now)
	require.NoError(t, err)

	authority, err := configtls.LoadAuthority(certPEM, keyPEM)
	require.NoError(t, err)

	return authority
}

func TestIssue_LeafPassesCheckForItsOwnName(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	require.NoError(t, authority.Check(certPEM, keyPEM, []string{planeName}, now))
}

func TestIssue_IPHostBecomesIPSAN(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{"192.0.2.10"}, now)
	require.NoError(t, err)

	require.NoError(t, authority.Check(certPEM, keyPEM, []string{"192.0.2.10"}, now))
}

func TestCheck_RejectsLeafOfAnotherPlane(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{"cf-proxy-other-config.tenant-b.svc.cluster.local"}, now)
	require.NoError(t, err)

	err = authority.Check(certPEM, keyPEM, []string{planeName}, now)
	require.Error(t, err)
	assert.NotErrorIs(t, err, configtls.ErrRenewalDue)
}

func TestCheck_RejectsLeafFromForeignAuthority(t *testing.T) {
	t.Parallel()

	ours := newAuthority(t)
	foreign := newAuthority(t)

	certPEM, keyPEM, err := foreign.Issue([]string{planeName}, now)
	require.NoError(t, err)

	err = ours.Check(certPEM, keyPEM, []string{planeName}, now)
	require.Error(t, err)
	assert.NotErrorIs(t, err, configtls.ErrRenewalDue)
}

func TestCheck_RejectsKeyThatDoesNotMatchCertificate(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, _, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	_, otherKeyPEM, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	require.Error(t, authority.Check(certPEM, otherKeyPEM, []string{planeName}, now))
}

func TestCheck_RejectsGarbage(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	require.Error(t, authority.Check([]byte("not a cert"), []byte("not a key"), []string{planeName}, now))
	require.Error(t, authority.Check(nil, nil, []string{planeName}, now))
}

func TestCheck_RenewalDueInsideWindow(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	fresh := now.Add(configtls.LeafValidity - configtls.RenewBefore - time.Hour)
	require.NoError(t, authority.Check(certPEM, keyPEM, []string{planeName}, fresh))

	due := now.Add(configtls.LeafValidity - configtls.RenewBefore + time.Hour)
	require.ErrorIs(t, authority.Check(certPEM, keyPEM, []string{planeName}, due), configtls.ErrRenewalDue)

	expired := now.Add(configtls.LeafValidity + time.Hour)
	require.Error(t, authority.Check(certPEM, keyPEM, []string{planeName}, expired))
}

func TestLoadAuthority_RejectsLeafAsCA(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	_, err = configtls.LoadAuthority(certPEM, keyPEM)
	require.Error(t, err)
}

func TestLoadAuthority_RejectsMismatchedKey(t *testing.T) {
	t.Parallel()

	certPEM, _, err := configtls.NewAuthorityPEM(now)
	require.NoError(t, err)

	_, otherKeyPEM, err := configtls.NewAuthorityPEM(now)
	require.NoError(t, err)

	_, err = configtls.LoadAuthority(certPEM, otherKeyPEM)
	require.Error(t, err)
}

func TestClientConfig_PinsTheAuthorityAndTheName(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)
	cfg := authority.ClientConfig(planeName)

	assert.Equal(t, planeName, cfg.ServerName)
	assert.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
	assert.False(t, cfg.InsecureSkipVerify)
	require.NotNil(t, cfg.RootCAs)
	assert.Len(t, cfg.RootCAs.Subjects(), 1, //nolint:staticcheck // counting pool entries, not reading system roots
		"the pool must hold the authority alone, never the system roots")
}

func TestIssue_LeafIsServerAuthOnlyAndNotCA(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)

	certPEM, _, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	assert.False(t, cert.IsCA)
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, cert.ExtKeyUsage)
	assert.Equal(t, []string{planeName}, cert.DNSNames)
}

func writePair(t *testing.T, dir string, certPEM, keyPEM []byte) (string, string) {
	t.Helper()

	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	return certFile, keyFile
}

func servedSerial(t *testing.T, loader *configtls.CertificateLoader) string {
	t.Helper()

	cert, err := loader.GetCertificate(nil)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	return leaf.SerialNumber.String()
}

func TestCertificateLoader_PicksUpRotatedFiles(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)
	dir := t.TempDir()

	firstCert, firstKey, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	certFile, keyFile := writePair(t, dir, firstCert, firstKey)

	loader, err := configtls.NewCertificateLoader(certFile, keyFile, nil)
	require.NoError(t, err)

	first := servedSerial(t, loader)

	secondCert, secondKey, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	writePair(t, dir, secondCert, secondKey)

	assert.NotEqual(t, first, servedSerial(t, loader))
}

func TestCertificateLoader_KeepsServingOnBrokenRotation(t *testing.T) {
	t.Parallel()

	authority := newAuthority(t)
	dir := t.TempDir()

	certPEM, keyPEM, err := authority.Issue([]string{planeName}, now)
	require.NoError(t, err)

	certFile, keyFile := writePair(t, dir, certPEM, keyPEM)

	loader, err := configtls.NewCertificateLoader(certFile, keyFile, nil)
	require.NoError(t, err)

	first := servedSerial(t, loader)

	require.NoError(t, os.WriteFile(certFile, []byte("half-written"), 0o600))

	assert.Equal(t, first, servedSerial(t, loader))
}

func TestNewCertificateLoader_RefusesUnreadablePair(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, err := configtls.NewCertificateLoader(filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key"), nil)
	require.Error(t, err)

	certFile, keyFile := writePair(t, dir, []byte("x"), []byte("y"))

	_, err = configtls.NewCertificateLoader(certFile, keyFile, nil)
	require.Error(t, err)
}

// TestTLS_EndToEndHandshake proves the client config and the loader agree
// on a real handshake, and that a leaf for another plane is refused.
func TestTLS_EndToEndHandshake(t *testing.T) {
	t.Parallel()

	wallClock := time.Now()

	caCertPEM, caKeyPEM, err := configtls.NewAuthorityPEM(wallClock)
	require.NoError(t, err)

	authority, err := configtls.LoadAuthority(caCertPEM, caKeyPEM)
	require.NoError(t, err)

	dir := t.TempDir()

	certPEM, keyPEM, err := authority.Issue([]string{planeName}, wallClock)
	require.NoError(t, err)

	certFile, keyFile := writePair(t, dir, certPEM, keyPEM)

	loader, err := configtls.NewCertificateLoader(certFile, keyFile, nil)
	require.NoError(t, err)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", loader.ServerConfig())
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()

	good, err := tls.Dial("tcp", listener.Addr().String(), authority.ClientConfig(planeName))
	require.NoError(t, err)
	require.NoError(t, good.Close())

	_, err = tls.Dial("tcp", listener.Addr().String(),
		authority.ClientConfig("cf-proxy-other-config.tenant-b.svc.cluster.local"))
	require.Error(t, err)
}
