package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const kindConfigMap = "ConfigMap"

// certRefGrantCase is one cross-namespace certificate reference of a
// non-Secret kind. The spec reserves InvalidCertificateRef and
// InvalidClientCertificateRef for a reference that is allowed, so the
// ReferenceGrant decides the reason before the kind does.
type certRefGrantCase struct {
	name       string
	grantTo    []gatewayv1beta1.ReferenceGrantTo
	wantDenied bool
}

func certRefGrantCases() []certRefGrantCase {
	return []certRefGrantCase{
		{name: "no grant", wantDenied: true},
		{
			name:       "grant to Secret does not cover a ConfigMap",
			grantTo:    []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: kindSecret}},
			wantDenied: true,
		},
		{
			name:       "grant to a ConfigMap of another name",
			grantTo:    []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: kindConfigMap, Name: new(gatewayv1.ObjectName("other"))}},
			wantDenied: true,
		},
		{
			name:    "grant to ConfigMap allows the reference",
			grantTo: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: kindConfigMap}},
		},
	}
}

func configMapCertRef() gatewayv1.SecretObjectReference {
	return gatewayv1.SecretObjectReference{
		Kind:      new(gatewayv1.Kind(kindConfigMap)),
		Namespace: new(gatewayv1.Namespace("certs")),
		Name:      "cert",
	}
}

func certRefGrantObjects(fromKind string, grantTo []gatewayv1beta1.ReferenceGrantTo) []client.Object {
	if grantTo == nil {
		return nil
	}

	return []client.Object{&gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "certs"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: gatewayv1.Kind(fromKind), Namespace: "default"},
			},
			To: grantTo,
		},
	}}
}

func wantCertRefReason(denied bool) string {
	if denied {
		return string(gatewayv1.ListenerReasonRefNotPermitted)
	}

	return string(gatewayv1.ListenerReasonInvalidCertificateRef)
}

func TestValidateSingleCertRef_CrossNamespaceKindChecksGrantFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range certRefGrantCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakeClient := setupGatewayFakeClientWithBeta1(certRefGrantObjects(kindGateway, tc.grantTo)...)
			reconciler := &GatewayReconciler{Client: fakeClient, Scheme: fakeClient.Scheme()}
			gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}

			status, reason, _, err := reconciler.validateSingleCertRef(context.Background(), gateway, configMapCertRef())
			require.NoError(t, err)
			assert.Equal(t, metav1.ConditionFalse, status)
			assert.Equal(t, wantCertRefReason(tc.wantDenied), reason)
		})
	}
}

func TestValidateListenerSetCertRef_CrossNamespaceKindChecksGrantFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range certRefGrantCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cli := buildTLSFakeClient(t, certRefGrantObjects(kindListenerSet, tc.grantTo)...)
			listenerSet := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"}}

			check, err := validateListenerSetCertRef(context.Background(), cli, listenerSet, configMapCertRef())
			require.NoError(t, err)
			assert.Equal(t, metav1.ConditionFalse, check.Status)
			assert.Equal(t, wantCertRefReason(tc.wantDenied), check.Reason)
		})
	}
}

func TestLoadGatewayClientCertPEM_CrossNamespaceKindChecksGrantFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range certRefGrantCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakeClient := setupGatewayFakeClientWithBeta1(certRefGrantObjects(kindGateway, tc.grantTo)...)
			ref := configMapCertRef()
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{TLS: &gatewayv1.GatewayTLSConfig{
					Backend: &gatewayv1.GatewayBackendTLS{ClientCertificateRef: &ref},
				}},
			}

			_, _, err := loadGatewayClientCertPEM(context.Background(), fakeClient, gateway,
				gatewayClientCertGrantChecker(fakeClient))

			want := errGatewayClientCertUnsupportedRef
			if tc.wantDenied {
				want = errGatewayClientCertRefNotPermitted
			}

			require.ErrorIs(t, err, want)
		})
	}
}

// A grant to a non-Secret kind can turn a RefNotPermitted certificate
// reference into InvalidCertificateRef, so it has to reach the owners.
func TestReferenceGrantMappers_EnqueueOnNonSecretGrant(t *testing.T) {
	t.Parallel()

	grantTo := []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: kindConfigMap}}

	t.Run("Gateway", func(t *testing.T) {
		t.Parallel()

		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Listeners: []gatewayv1.Listener{{
					Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{configMapCertRef()}},
				}},
			},
		}
		gatewayClass := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
		}
		grant := certRefGrantObjects(kindGateway, grantTo)[0]
		fakeClient := setupGatewayFakeClientWithBeta1(gateway, gatewayClass, grant)
		reconciler := &GatewayReconciler{Client: fakeClient, Scheme: fakeClient.Scheme(), ControllerName: "test-controller"}

		requests := reconciler.referenceGrantToGateways(context.Background(), grant)
		require.Len(t, requests, 1)
		assert.Equal(t, "gw", requests[0].Name)

		assert.Empty(t, reconciler.referenceGrantToGateways(context.Background(),
			certRefGrantObjects(kindListenerSet, grantTo)[0]), "a grant from ListenerSets does not concern a Gateway")
	})

	t.Run("ListenerSet", func(t *testing.T) {
		t.Parallel()

		listenerSet := &gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
			Spec: gatewayv1.ListenerSetSpec{
				Listeners: []gatewayv1.ListenerEntry{{
					Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{configMapCertRef()}},
				}},
			},
		}
		grant := certRefGrantObjects(kindListenerSet, grantTo)[0]
		cli := buildTLSFakeClient(t, listenerSet, grant)
		reconciler := &ListenerSetReconciler{Client: cli}

		requests := reconciler.referenceGrantToListenerSets(context.Background(), grant)
		require.Len(t, requests, 1)
		assert.Equal(t, "ls", requests[0].Name)

		assert.Empty(t, reconciler.referenceGrantToListenerSets(context.Background(),
			certRefGrantObjects(kindGateway, grantTo)[0]), "a grant from Gateways does not concern a ListenerSet")
	})
}
