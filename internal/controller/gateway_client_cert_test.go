package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// generateClientKeypair produces a self-signed EC keypair as PEM bytes,
// suitable for the data of a kubernetes.io/tls Secret. Returns (cert, key)
// in that order — positional, not named, to satisfy nonamedreturns.
func generateClientKeypair(t *testing.T) ([]byte, []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway-backend-client"},
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

// gatewayWithClientCertRef builds a Gateway whose spec.tls.backend.clientCertificateRef
// points at the named Secret. namespaceOverride is honoured when non-nil to model
// cross-namespace references.
func gatewayWithClientCertRef(
	namespace, name, secretName string,
	secretNamespace *gatewayv1.Namespace,
) *gatewayv1.Gateway {
	group := gatewayv1.Group("")
	kind := gatewayv1.Kind("Secret")

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.GatewaySpec{
			TLS: &gatewayv1.GatewayTLSConfig{
				Backend: &gatewayv1.GatewayBackendTLS{
					ClientCertificateRef: &gatewayv1.SecretObjectReference{
						Group:     &group,
						Kind:      &kind,
						Name:      gatewayv1.ObjectName(secretName),
						Namespace: secretNamespace,
					},
				},
			},
		},
	}
}

func clientCertSecret(namespace, name string, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}
}

func newClientCertScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	return scheme
}

// alwaysAllowGrant is the trivial "every cross-namespace reference is permitted"
// stub used by happy-path cases that don't model ReferenceGrant denial.
func alwaysAllowGrant(
	_ context.Context,
	_ *gatewayv1.Gateway,
	_ string,
	_ gatewayv1.SecretObjectReference,
) (bool, error) {
	return true, nil
}

// neverAllowGrant is the trivial "every cross-namespace reference is denied"
// stub used by the RefNotPermitted case.
func neverAllowGrant(
	_ context.Context,
	_ *gatewayv1.Gateway,
	_ string,
	_ gatewayv1.SecretObjectReference,
) (bool, error) {
	return false, nil
}

func TestLoadGatewayClientCertPEM_NoRef_ReturnsZero(t *testing.T) {
	t.Parallel()

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gw"}}

	certPEM, keyPEM, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.NoError(t, err)
	assert.Nil(t, certPEM)
	assert.Nil(t, keyPEM)
}

func TestLoadGatewayClientCertPEM_HappyPath_SameNamespace(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypair(t)
	secret := clientCertSecret("ns", "client-cert", certPEM, keyPEM)
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	gotCert, gotKey, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.NoError(t, err)
	assert.Equal(t, certPEM, gotCert)
	assert.Equal(t, keyPEM, gotKey)
}

func TestLoadGatewayClientCertPEM_CrossNamespace_GrantAllowed(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypair(t)
	secret := clientCertSecret("secret-ns", "client-cert", certPEM, keyPEM)
	secretNS := gatewayv1.Namespace("secret-ns")
	gateway := gatewayWithClientCertRef("gw-ns", "gw", "client-cert", &secretNS)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	gotCert, gotKey, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.NoError(t, err)
	assert.Equal(t, certPEM, gotCert)
	assert.Equal(t, keyPEM, gotKey)
}

func TestLoadGatewayClientCertPEM_CrossNamespace_GrantDenied(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypair(t)
	secret := clientCertSecret("secret-ns", "client-cert", certPEM, keyPEM)
	secretNS := gatewayv1.Namespace("secret-ns")
	gateway := gatewayWithClientCertRef("gw-ns", "gw", "client-cert", &secretNS)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, neverAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertRefNotPermitted)
}

func TestLoadGatewayClientCertPEM_SecretNotFound(t *testing.T) {
	t.Parallel()

	gateway := gatewayWithClientCertRef("ns", "gw", "missing-secret", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertSecretNotFound)
}

func TestLoadGatewayClientCertPEM_WrongSecretType(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := generateClientKeypair(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "opaque-cert"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}
	gateway := gatewayWithClientCertRef("ns", "gw", "opaque-cert", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertWrongType)
}

func TestLoadGatewayClientCertPEM_MissingTLSCrt(t *testing.T) {
	t.Parallel()

	_, keyPEM := generateClientKeypair(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "client-cert"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.key": keyPEM},
	}
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertMissingKey)
}

func TestLoadGatewayClientCertPEM_MissingTLSKey(t *testing.T) {
	t.Parallel()

	certPEM, _ := generateClientKeypair(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "client-cert"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certPEM},
	}
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertMissingKey)
}

func TestLoadGatewayClientCertPEM_InvalidPEM(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "client-cert"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("definitely not a PEM"),
			"tls.key": []byte("also not a PEM"),
		},
	}
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertInvalidPEM)
}

func TestLoadGatewayClientCertPEM_UnsupportedRefKind(t *testing.T) {
	t.Parallel()

	group := gatewayv1.Group("acme.example.com")
	kind := gatewayv1.Kind("ClientCertificate")
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gw"},
		Spec: gatewayv1.GatewaySpec{
			TLS: &gatewayv1.GatewayTLSConfig{
				Backend: &gatewayv1.GatewayBackendTLS{
					ClientCertificateRef: &gatewayv1.SecretObjectReference{
						Group: &group,
						Kind:  &kind,
						Name:  "client-cert",
					},
				},
			},
		},
	}

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, err := loadGatewayClientCertPEM(context.Background(), cli, gateway, alwaysAllowGrant)
	require.Error(t, err)
	assert.ErrorIs(t, err, errGatewayClientCertUnsupportedRef)
}

// Compile-time guard: signature must accept any client.Client.
var _ secretRefGrantChecker = alwaysAllowGrant

// Sanity-check that the sentinel errors are exported package-level vars,
// not literal allocations inside the function body (which would defeat
// errors.Is in callers).
func TestGatewayClientCertSentinels_AreStable(t *testing.T) {
	t.Parallel()

	// Each sentinel must be a distinct error value so errors.Is can tell
	// them apart in the controller status mapping layer.
	all := []error{
		errGatewayClientCertSecretNotFound,
		errGatewayClientCertWrongType,
		errGatewayClientCertMissingKey,
		errGatewayClientCertInvalidPEM,
		errGatewayClientCertUnsupportedRef,
		errGatewayClientCertRefNotPermitted,
	}

	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}

			assert.False(t, errors.Is(a, b),
				"sentinels at index %d and %d must not be the same error", i, j)
		}
	}
}

// Compile-time use of the fake client.Client interface so the import isn't
// pruned when sentinel tests grow.
var _ client.Client = (client.Client)(nil)
