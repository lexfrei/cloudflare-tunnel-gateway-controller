package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func clientCertValidationTLS() *gatewayv1.GatewayTLSConfig {
	return &gatewayv1.GatewayTLSConfig{
		Frontend: &gatewayv1.FrontendTLSConfig{
			Default: gatewayv1.TLSConfig{
				Validation: &gatewayv1.FrontendTLSValidation{
					CACertificateRefs: []gatewayv1.ObjectReference{{Kind: "ConfigMap", Name: "client-ca"}},
				},
			},
		},
	}
}

func TestGatewayReconciler_RefusesFrontendClientCertificateValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "mtls", Namespace: "default", Generation: 3},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
			TLS: clientCertValidationTLS(),
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
			TunnelID:                       "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
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

	fakeClient := setupGatewayFakeClient(gateway, secret, gatewayClassConfig, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	key := types.NamespacedName{Name: "mtls", Namespace: "default"}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, key, &updated))

	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalid), accepted.Reason)
	assert.Equal(t, int64(3), accepted.ObservedGeneration)
	assert.Contains(t, accepted.Message, "spec.tls.frontend")
	assert.Contains(t, accepted.Message, "Cloudflare edge", "the message must point to where client certificates can be checked")
	assert.LessOrEqual(t, len(accepted.Message), maxConditionMessageLength)
	assert.NotContains(t, accepted.Message, "...", "the message must fit without truncation")

	programmed := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalid), programmed.Reason)

	assert.Empty(t, updated.Status.Addresses, "a refused shared-plane Gateway advertises no tunnel address")

	require.Len(t, updated.Status.Listeners, 1)
	listenerProgrammed := meta.FindStatusCondition(updated.Status.Listeners[0].Conditions,
		string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, listenerProgrammed)
	assert.Equal(t, metav1.ConditionFalse, listenerProgrammed.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalid), listenerProgrammed.Reason)
}

func TestResolveRouteParentBinding_RefusesParentRequestingFrontendValidation(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
				},
			}},
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: namespacesFromAllPtr()},
			},
			TLS: clientCertValidationTLS(),
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{{
				Name: "ls-http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
				},
			}},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw, ls)
	validator := routebinding.NewValidator(cli)
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}
	parentNS := gatewayv1.Namespace("infra")

	for _, kind := range []gatewayv1.Kind{kindGateway, kindListenerSet} {
		name := gatewayv1.ObjectName("gw")
		if kind == kindListenerSet {
			name = "ls"
		}

		ref := gatewayv1.ParentReference{Kind: &kind, Name: name, Namespace: &parentNS}

		binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController,
			ref, "team-a", routeInfo, nil)
		require.NoError(t, err)
		assert.True(t, binding.ManagedByThisController, kind)
		assert.False(t, binding.Result.Accepted, kind)
		assert.Empty(t, binding.Result.MatchedListeners, kind)
		assert.Equal(t, routebinding.ParentRequestsFrontendValidationMessage, binding.Result.Message, kind)
	}
}

func TestListenerSetReconciler_ParentRequestingFrontendValidationIsNotAccepted(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: namespacesFromAllPtr()},
			},
			TLS: clientCertValidationTLS(),
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 2},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	r, cli := newListenerSetReconciler(t, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonParentNotAccepted), accepted.Reason)
	assert.Equal(t, routebinding.ParentRequestsFrontendValidationMessage, accepted.Message)
}
