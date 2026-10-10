package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

const classDefaultName = "tenant-plane"

// classFixturesWithDefault is classFixtures with the class naming
// classDefaultName as every Gateway's default GatewayConfig.
func classFixturesWithDefault() []runtime.Object {
	objects := classFixtures()

	for _, obj := range objects {
		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.PerGatewayDataPlanes = &v1alpha1.PerGatewayDataPlanes{
				DefaultGatewayConfigName: classDefaultName,
			}
		}
	}

	return objects
}

// gatewayWithoutRef is a Gateway of the fixture class that names no
// GatewayConfig itself.
func gatewayWithoutRef() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: testGwNamespace},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
	}
}

func defaultGatewayConfig() *v1alpha1.GatewayConfig {
	return &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: classDefaultName, Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}
}

func TestDataPlaneRef(t *testing.T) {
	t.Parallel()

	withDefault := &v1alpha1.GatewayClassConfig{Spec: v1alpha1.GatewayClassConfigSpec{
		PerGatewayDataPlanes: &v1alpha1.PerGatewayDataPlanes{DefaultGatewayConfigName: classDefaultName},
	}}
	explicit := gatewayWithInfra("example.com", "Other", "own")

	tests := []struct {
		name        string
		gateway     *gatewayv1.Gateway
		classConfig *v1alpha1.GatewayClassConfig
		want        *gatewayv1.LocalParametersReference
	}{
		{name: "no ref, no class config", gateway: gatewayWithoutRef()},
		{name: "no ref, class without default", gateway: gatewayWithoutRef(), classConfig: &v1alpha1.GatewayClassConfig{}},
		{
			name: "no ref, class with empty perGatewayDataPlanes", gateway: gatewayWithoutRef(),
			classConfig: &v1alpha1.GatewayClassConfig{Spec: v1alpha1.GatewayClassConfigSpec{
				PerGatewayDataPlanes: &v1alpha1.PerGatewayDataPlanes{},
			}},
		},
		{
			name: "no ref, class default", gateway: gatewayWithoutRef(), classConfig: withDefault,
			want: &gatewayv1.LocalParametersReference{
				Group: config.ParametersRefGroup, Kind: config.GatewayParametersRefKind, Name: classDefaultName,
			},
		},
		{
			name: "own ref replaces the class default, unvalidated", gateway: explicit, classConfig: withDefault,
			want: explicit.Spec.Infrastructure.ParametersRef,
		},
		{
			name: "own ref without class config", gateway: explicit,
			want: explicit.Spec.Infrastructure.ParametersRef,
		},
		{
			name: "infrastructure metadata alone is no ref", classConfig: withDefault,
			gateway: &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Infrastructure: &gatewayv1.GatewayInfrastructure{
				Labels: map[gatewayv1.LabelKey]gatewayv1.LabelValue{"a": "b"},
			}}},
			want: &gatewayv1.LocalParametersReference{
				Group: config.ParametersRefGroup, Kind: config.GatewayParametersRefKind, Name: classDefaultName,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, config.DataPlaneRef(tt.gateway, tt.classConfig))
		})
	}
}

func TestResolveForGateway_ClassDefaultGatewayConfig(t *testing.T) {
	t.Parallel()

	objects := append(classFixturesWithDefault(), defaultGatewayConfig(), tokenSecret(t), generatedAuthSecret())
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	require.NotNil(t, resolved, "a class default must give the Gateway a dedicated data plane")
	assert.Equal(t, testTunnelUUID, resolved.TunnelID)
	assert.Equal(t, "gatewayconfig:"+testGwNamespace+"/"+classDefaultName, resolved.ConfigName)
	assert.Equal(t, "generated-bearer", resolved.AuthToken)

	status, err := resolver.ResolveStatusConfigForGateway(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, testTunnelUUID, status.TunnelID)

	gwConfig, err := resolver.GetGatewayConfig(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	assert.Equal(t, classDefaultName, gwConfig.Name)
}

func TestResolveForGateway_OwnRefReplacesClassDefault(t *testing.T) {
	t.Parallel()

	own := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
		},
	}

	// No GatewayConfig of the default name exists: resolving it would fail.
	objects := append(classFixturesWithDefault(), own, tokenSecret(t), generatedAuthSecret())
	resolver := newGatewayResolver(t, objects...)

	resolved, err := resolver.ResolveForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	assert.Equal(t, "gatewayconfig:"+testGwNamespace+"/edge-config", resolved.ConfigName)
}

func TestResolveForGateway_MissingClassDefaultIsInvalidParameters(t *testing.T) {
	t.Parallel()

	resolver := newGatewayResolver(t, append(classFixturesWithDefault(), tokenSecret(t))...)

	_, err := resolver.ResolveStatusConfigForGateway(context.Background(), gatewayWithoutRef())
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrInvalidParameters)
	assert.Contains(t, err.Error(), "GatewayConfig "+testGwNamespace+"/"+classDefaultName)
	assert.Contains(t, err.Error(), `GatewayClass "cloudflare-tunnel"`,
		"the tenant set no parametersRef, so the message must say where the name came from")
}

func TestResolveForGateway_NoClassDefaultIsSharedPlane(t *testing.T) {
	t.Parallel()

	resolver := newGatewayResolver(t, classFixtures()...)

	resolved, err := resolver.ResolveForGateway(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	assert.Nil(t, resolved)
}

// A Gateway that names no GatewayConfig reads its class to learn whether it
// has a default; a failed read must be retried, not taken to mean "shared".
func TestUsesDedicatedPlane_ClassReadErrors(t *testing.T) {
	t.Parallel()

	builder := fake.NewClientBuilder().WithScheme(perGatewayScheme(t))
	for _, obj := range classFixturesWithDefault() {
		builder = builder.WithRuntimeObjects(obj)
	}

	builder = builder.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
				return errTransientAPIServer
			}

			return cli.Get(ctx, key, obj, opts...)
		},
	})

	resolver := config.NewResolver(builder.Build(), "cf-system", cfmetrics.NewNoopCollector())

	_, err := resolver.UsesDedicatedPlane(context.Background(), gatewayWithoutRef())
	require.Error(t, err)
	assert.NotErrorIs(t, err, config.ErrInvalidParameters)

	_, err = resolver.ResolveForGateway(context.Background(), gatewayWithoutRef())
	require.Error(t, err, "a ref-less Gateway must not resolve as shared while its class is unreadable")
	assert.NotErrorIs(t, err, config.ErrInvalidParameters)

	dedicated, err := resolver.UsesDedicatedPlane(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "x"))
	require.NoError(t, err, "a Gateway's own ref needs no class read")
	assert.True(t, dedicated)
}

func TestUsesDedicatedPlane(t *testing.T) {
	t.Parallel()

	withDefault := newGatewayResolver(t, classFixturesWithDefault()...)
	dedicated, err := withDefault.UsesDedicatedPlane(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	assert.True(t, dedicated)

	without := newGatewayResolver(t, classFixtures()...)
	dedicated, err = without.UsesDedicatedPlane(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	assert.False(t, dedicated)
}

func TestResolveTunnelClaimForGateway_ClassDefault(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofVerified}
	resolver := newClaimResolver(t, verifier,
		append(classFixturesWithDefault(), defaultGatewayConfig(), tokenSecret(t))...)

	claim, err := resolver.ResolveTunnelClaimForGateway(context.Background(), gatewayWithoutRef())
	require.NoError(t, err)
	require.NotNil(t, claim, "a class-default Gateway claims its tunnel like an explicit opt-in")
	assert.Equal(t, testTunnelUUID, claim.TunnelID)
	assert.Equal(t, tunnelownership.ProofVerified, claim.Proof)
}
