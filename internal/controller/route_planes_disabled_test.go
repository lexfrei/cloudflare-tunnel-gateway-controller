package controller

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

// TestResolveInfraGateways_PlanesDisabledRefusesWithoutResolving pins the
// route-sync half of --per-gateway-data-planes=false: every opted-in Gateway
// is refused (its routes are served from NO partition, never silently from
// the shared plane), and the refusal is decided without resolving the
// Gateway's GatewayConfig — its token Secret lives in a namespace the scoped
// Secret cache cannot read, so a resolve attempt would fail noisily on every
// sync instead of refusing cleanly once.
func TestResolveInfraGateways_PlanesDisabledRefusesWithoutResolving(t *testing.T) {
	t.Parallel()

	// perGatewayStatusFixtures WITHOUT the token Secret: a resolve attempt
	// would mark the Gateway broken-transient rather than refused, so the
	// assertion below also proves no resolve happened.
	fixtures := perGatewayStatusFixtures(t)
	objs := fixtures[:0:0]

	for _, obj := range fixtures {
		if obj.GetName() == "pg-token" {
			continue
		}

		objs = append(objs, obj)
	}

	fakeClient := setupGatewayFakeClient(objs...)
	syncer := NewRouteSyncer(fakeClient, fakeClient.Scheme(), "cluster.local", "test-controller",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)
	syncer.PerGatewayPlanesDisabled = true

	infra, err := syncer.resolveInfraGateways(context.Background())
	require.NoError(t, err)

	const key = "default/pg-gateway"

	assert.True(t, infra.isRefused(key), "an opted-in Gateway must be refused when planes are disabled")
	assert.Empty(t, infra.resolved, "no partition may exist for a refused Gateway")
	assert.False(t, infra.isTransient(key), "the refusal is deterministic, not a retryable blip")

	routeErr := gatewayPlaneError(key, infra)
	require.Error(t, routeErr, "a route bound to the refused Gateway must carry the refusal")
	assert.Contains(t, routeErr.Error(), "not programmed")
}

// TestApplyPlaneRefusals_PlanesDisabledSkipsArbitration pins that the
// disabled path never reaches tunnel-claim collection: collecting claims
// parses every opted-in Gateway's connector token, which reads Secrets the
// scoped cache cannot see. With everything already refused there is nothing
// to arbitrate.
//
// The invariant is asserted by COUNTING Secret reads through an interceptor,
// not inferred from the refused set: the token Secret exists in these
// fixtures, so if either disabled-path guard (resolveInfraGateways or
// applyPlaneRefusals) is deleted, the resolve/arbitration machinery reads it
// successfully and the zero-reads assertion is what fails.
func TestApplyPlaneRefusals_PlanesDisabledSkipsArbitration(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	var secretReads atomic.Int32

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(perGatewayStatusFixtures(t)...).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					secretReads.Add(1)
				}

				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isSecretList := list.(*corev1.SecretList); isSecretList {
					secretReads.Add(1)
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	syncer := NewRouteSyncer(fakeClient, fakeClient.Scheme(), "cluster.local", "test-controller",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)
	syncer.PerGatewayPlanesDisabled = true

	ctx := context.Background()

	infra, err := syncer.resolveInfraGateways(ctx)
	require.NoError(t, err)

	syncer.applyPlaneRefusals(ctx, infra, &config.ResolvedConfig{})

	assert.True(t, infra.isRefused("default/pg-gateway"))
	assert.Empty(t, infra.resolved)
	assert.Zero(t, secretReads.Load(),
		"the disabled path must read NO Secret: with the scoped cache they are unreadable, "+
			"and a read here means a dropped guard in resolveInfraGateways or applyPlaneRefusals")
}
