package config_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// resolvableClassObjects returns a GatewayClassConfig and its credentials
// Secret that every GatewayClass entry point resolves successfully, so a
// failure in the tests below can only come from the parametersRef itself.
func resolvableClassObjects() []client.Object {
	return []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-api-token")},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
				AccountID:                      "test-account-id",
				TunnelID:                       "12345678-1234-1234-1234-123456789abc",
			},
		},
	}
}

// Every entry point that reads a GatewayClass's parametersRef must refuse a
// namespace on it: GatewayClassConfig is cluster-scoped, and Gateway API
// requires the namespace to be unset for a cluster-scoped referent.
func TestGatewayClassEntryPoints_RejectParametersRefNamespace(t *testing.T) {
	t.Parallel()

	entryPoints := map[string]func(context.Context, *config.Resolver, *gatewayv1.GatewayClass) error{
		"ResolveFromGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveFromGatewayClass(ctx, gc)

			return errors.Wrap(err, "ResolveFromGatewayClass")
		},
		"ResolveFromGatewayClassName": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveFromGatewayClassName(ctx, gc.Name)

			return errors.Wrap(err, "ResolveFromGatewayClassName")
		},
		"ResolveTunnelPolicyForGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveTunnelPolicyForGatewayClass(ctx, gc.Name)

			return errors.Wrap(err, "ResolveTunnelPolicyForGatewayClass")
		},
		"GetConfigForGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.GetConfigForGatewayClass(ctx, gc)

			return errors.Wrap(err, "GetConfigForGatewayClass")
		},
	}

	for name, resolve := range entryPoints {
		t.Run(name+"/namespace set", func(t *testing.T) {
			t.Parallel()

			gatewayClass := newGatewayClass("test-class", "test-config")
			gatewayClass.Spec.ParametersRef.Namespace = new(gatewayv1.Namespace("default"))

			objs := append(resolvableClassObjects(), gatewayClass)
			resolver := config.NewResolver(setupFakeClient(objs...), "default", cfmetrics.NewNoopCollector())

			err := resolve(context.Background(), resolver, gatewayClass)

			require.Error(t, err)
			assert.True(t, errors.Is(err, config.ErrInvalidParameters), "want ErrInvalidParameters, got %v", err)
			assert.Contains(t, err.Error(), "namespace")
		})

		t.Run(name+"/namespace unset", func(t *testing.T) {
			t.Parallel()

			gatewayClass := newGatewayClass("test-class", "test-config")

			objs := append(resolvableClassObjects(), gatewayClass)
			resolver := config.NewResolver(setupFakeClient(objs...), "default", cfmetrics.NewNoopCollector())

			require.NoError(t, resolve(context.Background(), resolver, gatewayClass))
		})
	}
}
