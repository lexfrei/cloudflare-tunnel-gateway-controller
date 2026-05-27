package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	// Smallest realistic PEM-encoded test certificate. The PEM frame is what
	// validateListenerSetSecretExists checks; the contents don't have to
	// chain anywhere.
	testTLSCertPEM = "-----BEGIN CERTIFICATE-----\nMIIBaTCCAQ8CAQEwDQYJKoZIhvcNAQELBQAwDDEKMAgGA1UEAwwBYTAeFw0yNjAx\nMDEwMDAwMDBaFw0yNzAxMDEwMDAwMDBaMAwxCjAIBgNVBAMMAWEwgZ8wDQYJKoZI\nhvcNAQEBBQADgY0AMIGJAoGBAKQXXfwjlYIxNFhBPvLvXp4FrtRoVo3sxNGJSj1U\n5cFcQ8KqRtRpEKp0o5ZbWiXKD/IUOaCG3hb1uXm5LmKWuPFW9zhVPCfFCo2tKVu5\nyDgNPRfgQOL0avXjJgPVCm5pAxhJYHWvDgL9HBHo2C8FQywQT+CzMM0XPSDQ8DT0\nABcRAgMBAAEwDQYJKoZIhvcNAQELBQADAQA=\n-----END CERTIFICATE-----\n"
	testTLSKeyPEM  = "-----BEGIN PRIVATE KEY-----\nMIIBaTANBgkqhkiG9w0BAQEFAASCASYwggEiAgEAAoGBAKQXXfwjlYIxNFhBPvLv\n-----END PRIVATE KEY-----\n"
)

func TestResolveListenerEntryRefs_NoTLS(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}

	cli := buildTLSFakeClient(t)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, check.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonResolvedRefs), check.Reason)
}

func TestResolveListenerEntryRefs_SecretNotFound(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{Name: "missing"},
			},
		},
	}

	cli := buildTLSFakeClient(t)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, check.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), check.Reason)
}

func TestResolveListenerEntryRefs_WrongSecretType(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "wrong-type"}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wrong-type", Namespace: "ns"},
		Type:       corev1.SecretTypeOpaque,
	}

	cli := buildTLSFakeClient(t, secret)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, check.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), check.Reason)
	assert.Contains(t, check.Message, "kubernetes.io/tls")
}

func TestResolveListenerEntryRefs_MissingTLSKey(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cert-only"}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-only", Namespace: "ns"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte(testTLSCertPEM)},
	}

	cli := buildTLSFakeClient(t, secret)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, check.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), check.Reason)
	assert.Contains(t, check.Message, "tls.key")
}

func TestResolveListenerEntryRefs_HappyPath(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "good"}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "ns"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(testTLSCertPEM),
			corev1.TLSPrivateKeyKey: []byte(testTLSKeyPEM),
		},
	}

	cli := buildTLSFakeClient(t, secret)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, check.Status)
}

func TestResolveListenerEntryRefs_CrossNamespace_NoGrant(t *testing.T) {
	t.Parallel()

	otherNS := gatewayv1.Namespace("other")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{Name: "good", Namespace: &otherNS},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "other"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(testTLSCertPEM),
			corev1.TLSPrivateKeyKey: []byte(testTLSKeyPEM),
		},
	}

	cli := buildTLSFakeClient(t, secret)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, check.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonRefNotPermitted), check.Reason)
}

// TestResolveListenerEntryRefs_CrossNamespace_GatewayGrantDoesNotApply is the
// scoping guard the spec requires: ReferenceGrants applied to a Gateway are
// NOT inherited by child ListenerSets. A grant from Kind=Gateway must NOT
// permit a ListenerSet's TLS cert ref.
func TestResolveListenerEntryRefs_CrossNamespace_GatewayGrantDoesNotApply(t *testing.T) {
	t.Parallel()

	otherNS := gatewayv1.Namespace("other")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{Name: "good", Namespace: &otherNS},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "other"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(testTLSCertPEM),
			corev1.TLSPrivateKeyKey: []byte(testTLSKeyPEM),
		},
	}
	// Grant covers Gateway, not ListenerSet — must NOT cover the LS.
	gatewayGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-grant", Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: kindGateway, Namespace: "ns"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "", Kind: kindSecret},
			},
		},
	}

	cli := buildTLSFakeClient(t, secret, gatewayGrant)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, check.Status, "Gateway grant must NOT permit a ListenerSet reference")
	assert.Equal(t, string(gatewayv1.ListenerReasonRefNotPermitted), check.Reason)
}

// TestResolveListenerEntryRefs_CrossNamespace_ListenerSetGrantAllowed is the
// positive case: a grant from Kind=ListenerSet in the ListenerSet's namespace
// permits the cross-namespace cert ref.
func TestResolveListenerEntryRefs_CrossNamespace_ListenerSetGrantAllowed(t *testing.T) {
	t.Parallel()

	otherNS := gatewayv1.Namespace("other")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
	}
	entry := &gatewayv1.ListenerEntry{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{Name: "good", Namespace: &otherNS},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "other"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(testTLSCertPEM),
			corev1.TLSPrivateKeyKey: []byte(testTLSKeyPEM),
		},
	}
	lsGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "ls-grant", Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: kindListenerSet, Namespace: "ns"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "", Kind: kindSecret},
			},
		},
	}

	cli := buildTLSFakeClient(t, secret, lsGrant)

	check, err := resolveListenerEntryRefs(context.Background(), cli, ls, entry)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, check.Status, "ListenerSet-scoped grant must permit the cross-namespace cert ref")
}

func buildTLSFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithObjects(obj)
	}

	return builder.Build()
}
