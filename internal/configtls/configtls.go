// Package configtls holds the private certificate authority that protects
// the controller -> data plane config API. The controller owns the CA key;
// each data plane gets a leaf naming only its own config Service, and the
// controller's push client trusts that CA alone and checks the name, so a
// leaf held by one plane cannot answer for another.
package configtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
)

const (
	// CAValidity is the lifetime of the controller-generated CA. The CA is
	// never rotated automatically.
	CAValidity = 10 * 365 * 24 * time.Hour
	// LeafValidity is the lifetime of a data-plane serving certificate.
	LeafValidity = 365 * 24 * time.Hour
	// RenewBefore is how long before expiry a leaf is reissued.
	RenewBefore = 120 * 24 * time.Hour

	// clockSkew backdates NotBefore so a node whose clock lags the
	// controller's still accepts a certificate issued a moment ago.
	clockSkew = 5 * time.Minute

	serialBits = 128

	pemTypeCertificate = "CERTIFICATE"
	pemTypePrivateKey  = "PRIVATE KEY"
)

var (
	// ErrRenewalDue marks a leaf that is valid but inside the renewal window.
	ErrRenewalDue = errors.New("config API certificate is due for renewal")
	// ErrCAExpired marks a CA outside its validity period. Nothing it signs
	// verifies, so it must be replaced, not used.
	ErrCAExpired = errors.New("config API CA is expired or not yet valid")

	errNoCertificate = errors.New("no PEM certificate found")
	errNoPrivateKey  = errors.New("no PEM private key found")
	errNotCA         = errors.New("certificate is not a CA")
	errNotECDSA      = errors.New("private key is not ECDSA")
	errNoNames       = errors.New("a leaf needs at least one name")
)

// Authority is a loaded config API CA.
type Authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// NewAuthorityPEM generates a fresh CA keypair valid from now.
func NewAuthorityPEM(now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, errors.Wrap(err, "generating CA key")
	}

	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cloudflare-tunnel-gateway-controller config API CA"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "signing CA certificate")
	}

	return encode(der, key)
}

// LoadAuthority parses a CA keypair, refusing anything that is not a CA or
// whose key does not match the certificate.
func LoadAuthority(certPEM, keyPEM []byte) (*Authority, error) {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return nil, err
	}

	if !cert.IsCA {
		return nil, errNotCA
	}

	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}

	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("CA private key does not match the CA certificate")
	}

	return &Authority{cert: cert, key: key}, nil
}

// NotAfter returns the end of the CA's validity.
func (a *Authority) NotAfter() time.Time {
	return a.cert.NotAfter
}

// CheckValidAt reports ErrCAExpired when now is outside the CA's validity.
func (a *Authority) CheckValidAt(now time.Time) error {
	if now.Before(a.cert.NotBefore) || now.After(a.cert.NotAfter) {
		return errors.Wrapf(ErrCAExpired, "valid %s to %s",
			a.cert.NotBefore.UTC().Format(time.RFC3339), a.cert.NotAfter.UTC().Format(time.RFC3339))
	}

	return nil
}

// CertificatePEM returns the CA certificate.
func (a *Authority) CertificatePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: a.cert.Raw})
}

// Issue signs a serving certificate for names, valid from now and never past
// the CA's own expiry. A name that parses as an IP address becomes an IP SAN.
func (a *Authority) Issue(names []string, now time.Time) ([]byte, []byte, error) {
	if len(names) == 0 {
		return nil, nil, errNoNames
	}

	err := a.CheckValidAt(now)
	if err != nil {
		return nil, nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, errors.Wrap(err, "generating leaf key")
	}

	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     minTime(now.Add(LeafValidity), a.cert.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "signing leaf certificate")
	}

	return encode(der, key)
}

// Check reports whether a stored leaf is still fit to serve names: it must
// chain to this CA alone, carry every name, match its key, and not be inside
// the renewal window (ErrRenewalDue). Any other error means the leaf is
// unusable and must be replaced.
func (a *Authority) Check(certPEM, keyPEM []byte, names []string, now time.Time) error {
	_, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.Wrap(err, "certificate and key do not form a pair")
	}

	cert, err := parseCertificate(certPEM)
	if err != nil {
		return err
	}

	for _, name := range names {
		_, err := cert.Verify(x509.VerifyOptions{
			DNSName:     name,
			Roots:       a.pool(),
			CurrentTime: now,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return errors.Wrapf(err, "verifying certificate for %q", name)
		}
	}

	// A leaf already clamped to the CA's expiry cannot be extended by
	// reissuing it, so it is not reported due.
	if cert.NotAfter.Sub(now) < RenewBefore && cert.NotAfter.Before(a.cert.NotAfter) {
		return errors.Wrapf(ErrRenewalDue, "expires %s", cert.NotAfter.UTC().Format(time.RFC3339))
	}

	return nil
}

// ClientConfig returns the push client's TLS config for one data plane: this
// CA as the only root and serverName as the name the leaf must carry.
func (a *Authority) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		RootCAs:    a.pool(),
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}
}

func (a *Authority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)

	return pool
}

// CertificateLoader serves a keypair from files and follows them when they
// change, which is how kubelet delivers an updated Secret volume.
type CertificateLoader struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
	current *tls.Certificate
}

// NewCertificateLoader loads the pair once, failing when it is unreadable.
// A nil logger uses slog.Default.
func NewCertificateLoader(certFile, keyFile string, logger *slog.Logger) (*CertificateLoader, error) {
	if logger == nil {
		logger = slog.Default()
	}

	loader := &CertificateLoader{certFile: certFile, keyFile: keyFile, logger: logger}

	err := loader.reload()
	if err != nil {
		return nil, err
	}

	return loader, nil
}

// GetCertificate implements tls.Config.GetCertificate. It re-reads the files
// on every handshake and switches to a changed pair only once it parses, so a
// half-written update keeps the previous certificate in service.
func (l *CertificateLoader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	err := l.reload()
	if err != nil {
		l.logger.Warn("config API certificate update not applied; serving the previous one", "error", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.current, nil
}

// ServerConfig returns a TLS 1.3 server config backed by the loader.
func (l *CertificateLoader) ServerConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: l.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
}

func (l *CertificateLoader) reload() error {
	certPEM, err := os.ReadFile(l.certFile)
	if err != nil {
		return errors.Wrap(err, "reading config API certificate")
	}

	keyPEM, err := os.ReadFile(l.keyFile)
	if err != nil {
		return errors.Wrap(err, "reading config API key")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.current != nil && bytes.Equal(certPEM, l.certPEM) && bytes.Equal(keyPEM, l.keyPEM) {
		return nil
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.Wrap(err, "parsing config API keypair")
	}

	l.certPEM, l.keyPEM, l.current = certPEM, keyPEM, &pair

	return nil
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}

	return second
}

func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, errors.Wrap(err, "generating serial number")
	}

	return serial, nil
}

func encode(der []byte, key *ecdsa.PrivateKey) ([]byte, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "encoding private key")
	}

	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: keyDER}), nil
}

func parseCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCertificate {
		return nil, errNoCertificate
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "parsing certificate")
	}

	return cert, nil
}

func parseKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != pemTypePrivateKey {
		return nil, errNoPrivateKey
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "parsing private key")
	}

	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errNotECDSA
	}

	return key, nil
}
