package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// gatewayWithListeners builds a managed Gateway with the given listeners and the
// minimal GatewayClass/Config/Secret fixtures the reconciler needs.
func gatewayWithListenersFixture(listeners []gatewayv1.Listener) (*gatewayv1.Gateway, *corev1.Secret, *v1alpha1.GatewayClassConfig, *gatewayv1.GatewayClass) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        listeners,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	}
	gcc := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
			TunnelID:                       "12345678-1234-1234-1234-123456789abc",
		},
	}
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}

	return gateway, secret, gcc, gc
}

// reconcileGatewayListeners reconciles the fixture and returns the listener
// statuses written to the Gateway.
func reconcileGatewayListeners(t *testing.T, listeners []gatewayv1.Listener) []gatewayv1.ListenerStatus {
	t.Helper()

	gateway, secret, gcc, gc := gatewayWithListenersFixture(listeners)
	fakeClient := setupGatewayFakeClient(gateway, secret, gcc, gc)
	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway", Namespace: "default"}, &updated))

	return updated.Status.Listeners
}

// TestGatewayReconciler_UnsupportedListenerProtocol_AcceptedFalse pins that a
// listener whose protocol this controller cannot serve (TCP / TLS / UDP — there
// are no TCP/TLS/UDPRoute data planes; Cloudflare Tunnel is HTTP-focused) is
// marked Accepted=False / UnsupportedProtocol rather than the misleading
// Accepted=True it used to get unconditionally.
func TestGatewayReconciler_UnsupportedListenerProtocol_AcceptedFalse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		protocol gatewayv1.ProtocolType
	}{
		{"TCP", gatewayv1.TCPProtocolType},
		{"TLS", gatewayv1.TLSProtocolType},
		{"UDP", gatewayv1.UDPProtocolType},
		// INVALID (an unrecognised protocol) is the shape the conformance suite
		// uses; its default route kinds differ from TCP/TLS/UDP, so it exercises
		// the SupportedKinds reset separately.
		{"INVALID", gatewayv1.ProtocolType("INVALID")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{
				{Name: "l", Port: 443, Protocol: tc.protocol},
			})

			require.Len(t, statuses, 1)
			accepted := findCondition(statuses[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionFalse, accepted.Status,
				"a listener with an unservable protocol must be Accepted=False")
			assert.Equal(t, string(gatewayv1.ListenerReasonUnsupportedProtocol), accepted.Reason)
			assert.Contains(t, accepted.Message, string(tc.protocol), "message must name the unsupported protocol")
			assert.Empty(t, statuses[0].SupportedKinds,
				"a listener with an unservable protocol supports no route kinds")
		})
	}
}

// reconcileGatewayAccepted reconciles the fixture and returns the Gateway-level
// Accepted condition written to the Gateway.
func reconcileGatewayAccepted(t *testing.T, listeners []gatewayv1.Listener) *metav1.Condition {
	t.Helper()

	return findCondition(reconcileListenersGateway(t, listeners, nil).Status.Conditions,
		string(gatewayv1.GatewayConditionAccepted))
}

func reconcileListenersGateway(
	t *testing.T,
	listeners []gatewayv1.Listener,
	addresses []gatewayv1.GatewaySpecAddress,
	extra ...client.Object,
) gatewayv1.Gateway {
	t.Helper()

	gateway, secret, gcc, gc := gatewayWithListenersFixture(listeners)
	gateway.Spec.Addresses = addresses
	gateway.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{From: new(gatewayv1.NamespacesFromSame)},
	}
	fakeClient := setupGatewayFakeClient(append([]client.Object{gateway, secret, gcc, gc}, extra...)...)
	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway", Namespace: "default"}, &updated))

	return updated
}

// A Gateway the controller does not accept serves nothing, so it cannot
// claim Programmed=True; a partly valid Gateway is still programmed.
func TestGatewayReconciler_SharedPlane_ProgrammedFollowsAccepted(t *testing.T) {
	t.Parallel()

	const invalid = gatewayv1.ProtocolType("INVALID")

	attachedListenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "test-gateway"},
			Listeners: []gatewayv1.ListenerEntry{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}

	cases := []struct {
		name       string
		listeners  []gatewayv1.Listener
		addresses  []gatewayv1.GatewaySpecAddress
		extra      []client.Object
		wantStatus metav1.ConditionStatus
		wantReason gatewayv1.GatewayConditionReason
	}{
		{
			// The parent of a ListenerSet carries the merged listener list,
			// so the ListenerSet's entry is served.
			name:       "no valid own listener, valid ListenerSet attached",
			listeners:  []gatewayv1.Listener{{Name: "invalid", Port: 1111, Protocol: invalid}},
			extra:      []client.Object{attachedListenerSet},
			wantStatus: metav1.ConditionTrue,
			wantReason: gatewayv1.GatewayReasonProgrammed,
		},
		{
			name:       "no valid listener",
			listeners:  []gatewayv1.Listener{{Name: "invalid", Port: 1111, Protocol: invalid}},
			wantStatus: metav1.ConditionFalse,
			wantReason: gatewayv1.GatewayReasonInvalid,
		},
		{
			name:      "no valid listener outranks an unusable address",
			listeners: []gatewayv1.Listener{{Name: "invalid", Port: 1111, Protocol: invalid}},
			addresses: []gatewayv1.GatewaySpecAddress{
				{Type: new(gatewayv1.HostnameAddressType), Value: "other.example.com"},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: gatewayv1.GatewayReasonInvalid,
		},
		{
			name: "some valid listener",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "invalid", Port: 1111, Protocol: invalid},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: gatewayv1.GatewayReasonProgrammed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			updated := reconcileListenersGateway(t, tc.listeners, tc.addresses, tc.extra...)
			accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			require.NotNil(t, accepted)
			require.NotNil(t, programmed)
			assert.Equal(t, tc.wantStatus, programmed.Status)
			assert.Equal(t, string(tc.wantReason), programmed.Reason)

			if tc.wantStatus == metav1.ConditionFalse {
				assert.Equal(t, accepted.Message, programmed.Message,
					"Programmed must say why the Gateway was not accepted")
			}
		})
	}
}

// TestGatewayReconciler_UnsupportedProtocol_GatewayAcceptedReflectsListeners pins
// the Gateway-level Accepted condition to the validity of its listeners, per the
// Gateway API spec (a Gateway with any invalid listener is ListenersNotValid, and
// a Gateway with no valid listener at all is Accepted=False). Exercises the
// GatewayListenerUnsupportedProtocol conformance shape (protocol: INVALID).
func TestGatewayReconciler_UnsupportedProtocol_GatewayAcceptedReflectsListeners(t *testing.T) {
	t.Parallel()

	const invalid = gatewayv1.ProtocolType("INVALID")

	cases := []struct {
		name       string
		listeners  []gatewayv1.Listener
		wantStatus metav1.ConditionStatus
		wantReason gatewayv1.GatewayConditionReason
	}{
		{
			name:       "all listeners unsupported",
			listeners:  []gatewayv1.Listener{{Name: "invalid", Port: 1111, Protocol: invalid}},
			wantStatus: metav1.ConditionFalse,
			wantReason: gatewayv1.GatewayReasonListenersNotValid,
		},
		{
			name: "mixed supported and unsupported",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "invalid", Port: 1111, Protocol: invalid},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: gatewayv1.GatewayReasonListenersNotValid,
		},
		{
			name:       "all listeners supported",
			listeners:  []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
			wantStatus: metav1.ConditionTrue,
			wantReason: gatewayv1.GatewayReasonAccepted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			accepted := reconcileGatewayAccepted(t, tc.listeners)
			require.NotNil(t, accepted)
			assert.Equal(t, tc.wantStatus, accepted.Status)
			assert.Equal(t, string(tc.wantReason), accepted.Reason)
		})
	}
}

// TestGatewayReconciler_HTTPListener_Accepted confirms the happy path: an HTTP
// (and HTTPS) listener stays Accepted=True — those carry HTTPRoute / GRPCRoute
// which the in-process proxy serves.
func TestGatewayReconciler_HTTPListener_Accepted(t *testing.T) {
	t.Parallel()

	for _, proto := range []gatewayv1.ProtocolType{gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType} {
		statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{
			{Name: "l", Port: 80, Protocol: proto},
		})

		require.Len(t, statuses, 1)
		accepted := findCondition(statuses[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
		require.NotNil(t, accepted)
		assert.Equal(t, metav1.ConditionTrue, accepted.Status,
			"%s listener must stay Accepted=True", proto)
	}
}
