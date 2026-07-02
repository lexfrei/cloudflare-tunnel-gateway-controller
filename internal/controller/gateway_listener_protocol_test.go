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
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		})
	}
}

// reconcileGatewayAccepted reconciles the fixture and returns the Gateway-level
// Accepted condition written to the Gateway.
func reconcileGatewayAccepted(t *testing.T, listeners []gatewayv1.Listener) *metav1.Condition {
	t.Helper()

	gateway, secret, gcc, gc := gatewayWithListenersFixture(listeners)
	fakeClient := setupGatewayFakeClient(gateway, secret, gcc, gc)
	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway", Namespace: "default"}, &updated))

	return findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
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
