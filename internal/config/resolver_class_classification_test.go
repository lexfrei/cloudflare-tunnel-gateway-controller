package config_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// classEntryPoint runs one Resolver entry point that reads a GatewayClass's
// parametersRef and returns only its error.
type classEntryPoint func(context.Context, *config.Resolver, *gatewayv1.GatewayClass) error

// classEntryPoints are every Resolver entry point that reads a GatewayClass's
// parametersRef.
func classEntryPoints() map[string]classEntryPoint {
	return map[string]classEntryPoint{
		"ResolveFromGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveFromGatewayClass(ctx, gc)

			return err //nolint:wrapcheck // the test inspects the error as returned
		},
		"ResolveFromGatewayClassName": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveFromGatewayClassName(ctx, gc.Name)

			return err //nolint:wrapcheck // the test inspects the error as returned
		},
		"ResolveTunnelPolicyForGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.ResolveTunnelPolicyForGatewayClass(ctx, gc.Name)

			return err //nolint:wrapcheck // the test inspects the error as returned
		},
		"GetConfigForGatewayClass": func(ctx context.Context, r *config.Resolver, gc *gatewayv1.GatewayClass) error {
			_, err := r.GetConfigForGatewayClass(ctx, gc)

			return err //nolint:wrapcheck // the test inspects the error as returned
		},
	}
}

// TestGatewayClassEntryPoints_ClassifyConfigurationErrors pins one
// classification for every entry point: a problem with the GatewayClass's
// parametersRef or the objects it names is ErrInvalidParameters, and the
// message names the GatewayClass as its source, never a Gateway's
// spec.infrastructure.parametersRef. The tunnel policy leaves a missing
// GatewayClassConfig unclassified: it decides tunnel ownership, and the config
// may be mid-apply.
func TestGatewayClassEntryPoints_ClassifyConfigurationErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]func(gc *gatewayv1.GatewayClass) []client.Object{
		"no parametersRef": func(gc *gatewayv1.GatewayClass) []client.Object {
			gc.Spec.ParametersRef = nil

			return resolvableClassObjects()
		},
		"wrong kind": func(gc *gatewayv1.GatewayClass) []client.Object {
			gc.Spec.ParametersRef.Kind = "ConfigMap"

			return resolvableClassObjects()
		},
		"namespace set": func(gc *gatewayv1.GatewayClass) []client.Object {
			gc.Spec.ParametersRef.Namespace = new(gatewayv1.Namespace("default"))

			return resolvableClassObjects()
		},
		"GatewayClassConfig does not exist": func(*gatewayv1.GatewayClass) []client.Object {
			return nil
		},
	}

	for entryName, resolve := range classEntryPoints() {
		for caseName, setup := range cases {
			t.Run(entryName+"/"+caseName, func(t *testing.T) {
				t.Parallel()

				gatewayClass := newGatewayClass("test-class", "test-config")
				objs := append(setup(gatewayClass), gatewayClass)
				resolver := config.NewResolver(setupFakeClient(objs...), "default", cfmetrics.NewNoopCollector())

				err := resolve(context.Background(), resolver, gatewayClass)
				require.Error(t, err)

				classified := entryName != "ResolveTunnelPolicyForGatewayClass" || caseName != "GatewayClassConfig does not exist"
				assert.Equal(t, classified, errors.Is(err, config.ErrInvalidParameters), "classified: %v", err)
				assert.Contains(t, err.Error(), `GatewayClass "test-class"`)
				assert.NotContains(t, err.Error(), "infrastructure")
			})
		}
	}
}

// TestGatewayClassEntryPoints_ReadFailureIsNotClassified pins the other side
// for every entry point: a read that fails for a reason other than NotFound
// says nothing about the configuration.
func TestGatewayClassEntryPoints_ReadFailureIsNotClassified(t *testing.T) {
	t.Parallel()

	for entryName, resolve := range classEntryPoints() {
		t.Run(entryName, func(t *testing.T) {
			t.Parallel()

			gatewayClass := newGatewayClass("test-class", "test-config")

			scheme := runtime.NewScheme()
			utilruntime.Must(clientgoscheme.AddToScheme(scheme))
			utilruntime.Must(gatewayv1.Install(scheme))
			utilruntime.Must(v1alpha1.AddToScheme(scheme))

			cli := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append(resolvableClassObjects(), gatewayClass)...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
							return errReadFailed
						}

						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			err := resolve(context.Background(), config.NewResolver(cli, "default", cfmetrics.NewNoopCollector()), gatewayClass)
			require.Error(t, err)

			assert.ErrorIs(t, err, errReadFailed)
			assert.NotErrorIs(t, err, config.ErrInvalidParameters, "a read failure is not a configuration problem")
		})
	}
}
