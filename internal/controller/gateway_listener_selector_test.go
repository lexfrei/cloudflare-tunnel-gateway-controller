package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	return reconcileSelectorGatewayWith(t, gatewayv1.GatewaySpec{Listeners: listeners}, nil)
}

// reconcileSelectorGatewayWith reconciles a class-chain Gateway with the given
// spec, recording Events on recorder when it is not nil, and returns it as
// stored.
func reconcileSelectorGatewayWith(t *testing.T, spec gatewayv1.GatewaySpec, recorder events.EventRecorder) gatewayv1.Gateway {
	t.Helper()

	spec.GatewayClassName = "cloudflare-tunnel"
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default", Generation: 1},
		Spec:       spec,
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
		Recorder:       recorder,
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

// TestGatewayEvents_InvalidAllowedListenersSelector pins the signal for a
// Gateway whose allowedListeners.namespaces.selector does not parse. No
// Gateway API condition describes it, and the Gateway's own listeners still
// serve, so it is a Warning Event on the Gateway rather than a condition. Every
// ListenerSet is refused and says so on its own status. The Event says the
// selector is invalid without quoting it.
func TestGatewayEvents_InvalidAllowedListenersSelector(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	fromAll := gatewayv1.NamespacesFromAll
	listeners := []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}

	for _, tt := range []struct {
		name     string
		from     gatewayv1.FromNamespaces
		selector *metav1.LabelSelector
		want     int
	}{
		{name: "selector does not parse", from: fromSelector, selector: bogusNamespaceSelector(), want: 1},
		{name: "selector parses", from: fromSelector, selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}, want: 0},
		{name: "unparseable selector beside From All is not read", from: fromAll, selector: bogusNamespaceSelector(), want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := events.NewFakeRecorder(10)

			reconcileSelectorGatewayWith(t, gatewayv1.GatewaySpec{
				Listeners: listeners,
				AllowedListeners: &gatewayv1.AllowedListeners{
					Namespaces: &gatewayv1.ListenerNamespaces{From: &tt.from, Selector: tt.selector},
				},
			}, recorder)

			var warnings []string

			for _, event := range drainEvents(recorder) {
				if strings.Contains(event, "InvalidAllowedListeners") {
					warnings = append(warnings, event)
				}
			}

			require.Len(t, warnings, tt.want)

			for _, event := range warnings {
				assert.Contains(t, event, "Warning")
				assert.Contains(t, event, "allowedListeners.namespaces.selector is invalid; every ListenerSet is refused")
				assert.NotContains(t, event, "BogusOperator", "the Event says the selector is invalid, not what it is")
			}
		})
	}
}

// TestGatewayListenerStatus_AllListenersInvalid pins the Gateway verdict when
// no listener is valid: Accepted=False with ListenersNotValid, for a bad
// selector on every listener and for a bad selector next to an unsupported
// protocol, whose message names both causes.
func TestGatewayListenerStatus_AllListenersInvalid(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	badSelector := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: &fromSelector, Selector: bogusNamespaceSelector()},
	}

	for _, tt := range []struct {
		name      string
		listeners []gatewayv1.Listener
		causes    []string
	}{
		{
			name: "every listener has a bad selector",
			listeners: []gatewayv1.Listener{
				{Name: "a", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: badSelector},
				{Name: "b", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: badSelector},
			},
			causes: []string{"allowedRoutes.namespaces.selector"},
		},
		{
			name: "a bad selector beside an unsupported protocol",
			listeners: []gatewayv1.Listener{
				{Name: "a", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: badSelector},
				{Name: "b", Port: 9000, Protocol: gatewayv1.TCPProtocolType},
			},
			causes: []string{"allowedRoutes.namespaces.selector", "protocol"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			updated := reconcileSelectorGateway(t, tt.listeners)

			accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionFalse, accepted.Status, "no listener is valid")
			assert.Equal(t, string(gatewayv1.GatewayReasonListenersNotValid), accepted.Reason)

			for _, cause := range tt.causes {
				assert.Contains(t, accepted.Message, cause)
			}

			assert.NotContains(t, accepted.Message, "BogusOperator")
		})
	}
}
