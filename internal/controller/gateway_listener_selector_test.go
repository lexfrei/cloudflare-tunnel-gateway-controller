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

// bogusNamespaceSelector is a label selector the API server accepts and
// metav1.LabelSelectorAsSelector refuses to parse.
func bogusNamespaceSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: "BogusOperator", Values: []string{"x"}},
		},
	}
}

// reconcileSelectorGateway reconciles a class-chain Gateway with the given
// listeners and returns it as stored.
func reconcileSelectorGateway(t *testing.T, listeners []gatewayv1.Listener) gatewayv1.Gateway {
	t.Helper()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default", Generation: 1},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: listeners},
	}

	fakeClient := setupGatewayFakeClient(gateway,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
				TunnelID:                       "12345678-1234-1234-1234-123456789abc",
			},
		},
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "test-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "test-config",
				},
			},
		},
	)

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

func listenerStatusNamed(t *testing.T, gateway *gatewayv1.Gateway, name gatewayv1.SectionName) gatewayv1.ListenerStatus {
	t.Helper()

	for _, status := range gateway.Status.Listeners {
		if status.Name == name {
			return status
		}
	}

	require.Failf(t, "no listener status", "listener %s", name)

	return gatewayv1.ListenerStatus{}
}

// TestGatewayListenerStatus_InvalidAllowedRoutesSelector pins that a listener
// whose allowedRoutes.namespaces.selector does not parse says so on its own
// status, where the Gateway owner looks. The listener is not semantically
// valid and admits no route, so it is not Accepted and not Programmed; the
// message says the selector is invalid without quoting it. A sibling listener
// is unaffected, so the Gateway stays Accepted with ListenersNotValid.
func TestGatewayListenerStatus_InvalidAllowedRoutesSelector(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector

	updated := reconcileSelectorGateway(t, []gatewayv1.Listener{
		{
			Name: "broken", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &fromSelector, Selector: bogusNamespaceSelector()},
			},
		},
		{Name: "healthy", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
	})

	broken := listenerStatusNamed(t, &updated, "broken")

	accepted := findCondition(broken.Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonUnsupportedValue), accepted.Reason)
	assert.Contains(t, accepted.Message, "allowedRoutes.namespaces.selector is invalid")
	assert.NotContains(t, accepted.Message, "BogusOperator", "the status says the selector is invalid, not what it is")

	programmed := findCondition(broken.Conditions, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalid), programmed.Reason)

	healthy := listenerStatusNamed(t, &updated, "healthy")
	healthyAccepted := findCondition(healthy.Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, healthyAccepted)
	assert.Equal(t, metav1.ConditionTrue, healthyAccepted.Status)

	gatewayAccepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, gatewayAccepted)
	assert.Equal(t, metav1.ConditionTrue, gatewayAccepted.Status, "the healthy listener still serves")
	assert.Equal(t, string(gatewayv1.GatewayReasonListenersNotValid), gatewayAccepted.Reason)
	assert.NotContains(t, gatewayAccepted.Message, "BogusOperator")
}

// TestGatewayListenerStatus_UnusedSelectorIsIgnored pins that a selector only
// counts when allowedRoutes.namespaces.from is Selector, the one mode that
// reads it.
func TestGatewayListenerStatus_UnusedSelectorIsIgnored(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame

	updated := reconcileSelectorGateway(t, []gatewayv1.Listener{
		{
			Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame, Selector: bogusNamespaceSelector()},
			},
		},
	})

	accepted := findCondition(listenerStatusNamed(t, &updated, "same").Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
}
