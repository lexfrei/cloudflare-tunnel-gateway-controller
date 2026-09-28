package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// The Gateway's InvalidParameters message names where the bad configuration
// is: the GatewayClass's spec.parametersRef chain, or the Gateway's own
// spec.infrastructure.parametersRef. Blaming the wrong one sends the reader to
// an object that is fine.

// TestGatewayConfigError_ClassSourceNamed pins a Gateway without its own data
// plane whose GatewayClass names a GatewayClassConfig that does not exist.
func TestGatewayConfigError_ClassSourceNamed(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
		},
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "missing-config",
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, gatewayClass)
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

	accepted := acceptedConditionOf(t, fakeClient, "test-gateway")
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.Contains(t, accepted.Message, `GatewayClass "cloudflare-tunnel"`)
	assert.NotContains(t, accepted.Message, "infrastructure")
}

// TestGatewayConfigError_InfrastructureSourceNamed pins a Gateway with its own
// data plane whose infrastructure.parametersRef names a GatewayConfig that
// does not exist.
func TestGatewayConfigError_InfrastructureSourceNamed(t *testing.T) {
	t.Parallel()

	var objects []client.Object

	for _, obj := range perGatewayStatusFixtures(t) {
		if _, isGatewayConfig := obj.(*v1alpha1.GatewayConfig); isGatewayConfig {
			continue
		}

		objects = append(objects, obj)
	}

	updated := reconcilePGGateway(t, setupGatewayFakeClient(objects...))

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.Contains(t, accepted.Message, "spec.infrastructure.parametersRef")
}

// TestClassConfigConflict_DoesNotBlameTheGateway pins that the class conflict,
// a problem between GatewayClasses, does not name a Gateway's
// infrastructure.parametersRef.
func TestClassConfigConflict_DoesNotBlameTheGateway(t *testing.T) {
	t.Parallel()

	classFor := func(name, configName string) gatewayv1.GatewayClass {
		return gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "test-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: configName,
				},
			},
		}
	}

	err := classConfigConflict([]gatewayv1.GatewayClass{classFor("a", "one"), classFor("b", "two")}, "test-controller")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "infrastructure")
	assert.ErrorIs(t, err, config.ErrInvalidParameters, "the conflict is visible to the standard library as well")
}

// acceptedConditionOf reads a Gateway in the default namespace and returns its
// Accepted condition.
func acceptedConditionOf(t *testing.T, cli client.Client, name string) *metav1.Condition {
	t.Helper()

	var gateway gatewayv1.Gateway
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &gateway))

	accepted := findCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)

	return accepted
}

// TestResolveConfigForController_ReadFailureNamesTheClass pins that an
// unclassified failure resolving the class configuration still says which
// GatewayClass was being resolved. A classified one names it on its own.
func TestResolveConfigForController_ReadFailureNamesTheClass(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "a-class"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "cfg",
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayClass).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
					return errTransientRead
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	syncer := NewRouteSyncer(cli, scheme, "cluster.local", "test-controller",
		config.NewResolver(cli, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)

	_, err := syncer.resolveConfigForController(context.Background())
	require.ErrorIs(t, err, errTransientRead)
	assert.NotErrorIs(t, err, config.ErrInvalidParameters)
	assert.Contains(t, err.Error(), "a-class")
}
