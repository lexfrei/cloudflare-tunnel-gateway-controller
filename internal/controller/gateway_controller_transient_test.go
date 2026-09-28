package controller

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// TestGatewayReconciler_TransientResolveErrorKeepsSharedPlaneStatus pins that
// a read failure while resolving a shared-plane Gateway's configuration leaves
// its last status in place. The address is what external-dns publishes, so
// clearing it on a read failure would drop DNS for every hostname on the
// Gateway. The reconcile returns the error instead, for a backoff retry.
func TestGatewayReconciler_TransientResolveErrorKeepsSharedPlaneStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		failOn func(obj any, list bool) bool
	}{
		{
			name: "class-conflict listing fails",
			failOn: func(obj any, list bool) bool {
				_, ok := obj.(*gatewayv1.GatewayClassList)

				return list && ok
			},
		},
		{
			name: "credentials Secret read fails",
			failOn: func(obj any, list bool) bool {
				_, ok := obj.(*corev1.Secret)

				return !list && ok
			},
		},
		{
			name: "GatewayClassConfig read fails",
			failOn: func(obj any, list bool) bool {
				_, ok := obj.(*v1alpha1.GatewayClassConfig)

				return !list && ok
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()

			var armed atomic.Bool

			fail := func(obj any, list bool) bool {
				return armed.Load() && tt.failOn(obj, list) && armed.CompareAndSwap(true, false)
			}

			fakeClient := sharedPlaneGatewayClient(t, interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if fail(obj, false) {
						return errTransientRead
					}

					return cli.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if fail(list, true) {
						return errTransientRead
					}

					return cli.List(ctx, list, opts...)
				},
			})

			reconciler := &GatewayReconciler{
				Client:         fakeClient,
				Scheme:         fakeClient.Scheme(),
				ControllerName: "test-controller",
				ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
			}

			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"}}

			_, err := reconciler.Reconcile(ctx, request)
			require.NoError(t, err)

			armed.Store(true)

			_, err = reconciler.Reconcile(ctx, request)
			require.ErrorIs(t, err, errTransientRead, "a read failure is retried with backoff")
			assert.False(t, armed.Load(), "the injected failure must have fired")

			var updated gatewayv1.Gateway
			require.NoError(t, fakeClient.Get(ctx, request.NamespacedName, &updated))

			require.Len(t, updated.Status.Addresses, 1, "a read failure must not clear the address")

			accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionTrue, accepted.Status, "a read failure is not a bad parameter")
		})
	}
}

// sharedPlaneGatewayClient serves a class-chain Gateway with a resolvable
// GatewayClassConfig and credentials Secret.
func sharedPlaneGatewayClient(t *testing.T, funcs interceptor.Funcs) client.WithWatch {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
				},
			},
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
						Group: config.ParametersRefGroup,
						Kind:  config.ParametersRefKind,
						Name:  "test-config",
					},
				},
			},
		).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		WithInterceptorFuncs(funcs).
		Build()
}
