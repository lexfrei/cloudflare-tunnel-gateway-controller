package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

const ownershipLabelKey = "cf.k8s.lex.la/hostname-suffix"

// ownershipHTTPRoute builds a route in namespace targeting test-gateway with
// the given hostnames.
func ownershipHTTPRoute(namespace, name string, hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
	gwNamespace := gatewayv1.Namespace("default")

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "test-gateway", Namespace: &gwNamespace}},
			},
			Hostnames: hostnames,
		},
	}
}

// newOwnershipSyncer assembles a RouteSyncer with the controller-side
// hostname-ownership layer enabled and a Gateway whose listener allows routes
// from all namespaces (the permissive setup the policy exists to harden).
func newOwnershipSyncer(t *testing.T, objects ...runtime.Object) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fromAll := gatewayv1.NamespacesFromAll

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
					},
				},
			},
		},
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel"},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway, gatewayClass)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	fakeClient := builder.Build()

	syncer := NewRouteSyncer(
		fakeClient,
		scheme,
		"cluster.local",
		"cloudflare-tunnel",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		cfmetrics.NewNoopCollector(),
		nil,
	)

	policy, err := hostnameownership.New(ownershipLabelKey, "")
	require.NoError(t, err)

	syncer.HostnameOwnership = policy

	return syncer
}

// TestRouteSyncer_HostnameOwnership_RejectsViolatingRoute pins the
// controller-side enforcement layer (#475, defence-in-depth): even with NO
// admission policy installed, a route claiming a hostname outside its
// namespace's allowed suffix is never accepted — it lands in rejected with
// Accepted=False/HostnameNotPermitted and is excluded from the data plane.
func TestRouteSyncer_HostnameOwnership_RejectsViolatingRoute(t *testing.T) {
	t.Parallel()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "team-b",
		Labels: map[string]string{ownershipLabelKey: "team-b.example.com"},
	}}
	inside := ownershipHTTPRoute("team-b", "own-host", "app.team-b.example.com")
	violating := ownershipHTTPRoute("team-b", "capture-attempt", "app.team-a.example.com")

	syncer := newOwnershipSyncer(t, namespace, inside, violating)

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)

	require.Len(t, result.accepted, 1, "only the in-suffix route may be programmed")
	assert.Equal(t, "own-host", result.accepted[0].Name)

	require.Len(t, result.rejected, 1)
	assert.Equal(t, "capture-attempt", result.rejected[0].Name)

	binding := result.bindings["team-b/capture-attempt"]
	require.NotEmpty(t, binding.bindingResults)

	for _, bindingResult := range binding.bindingResults {
		assert.False(t, bindingResult.Accepted)
		assert.Equal(t, hostnameownership.RouteReasonHostnameNotPermitted, bindingResult.Reason)
		assert.NotEmpty(t, bindingResult.Message)
	}

	assert.Empty(t, binding.acceptedGateways, "a denied route must not count as accepted on any Gateway")
}

// TestRouteSyncer_HostnameOwnership_FailClosedOnUnlabelledNamespace pins the
// fail-closed contract: a policed namespace without the ownership label
// cannot program ANY route, including hostname-less ones (which would inherit
// the listener hostname — the capture vector).
func TestRouteSyncer_HostnameOwnership_FailClosedOnUnlabelledNamespace(t *testing.T) {
	t.Parallel()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-x"}}
	hostless := ownershipHTTPRoute("team-x", "no-hostnames")

	syncer := newOwnershipSyncer(t, namespace, hostless)

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)

	assert.Empty(t, result.accepted)
	require.Len(t, result.rejected, 1)
}

// TestRouteSyncer_HostnameOwnership_DisabledKeepsCurrentBehaviour pins the
// default-off contract: without a policy, the permissive setup keeps working
// exactly as before (conformance depends on this).
func TestRouteSyncer_HostnameOwnership_DisabledKeepsCurrentBehaviour(t *testing.T) {
	t.Parallel()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}}
	route := ownershipHTTPRoute("team-b", "any-host", "app.team-a.example.com")

	syncer := newOwnershipSyncer(t, namespace, route)
	syncer.HostnameOwnership = nil

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)

	assert.Len(t, result.accepted, 1)
	assert.Empty(t, result.rejected)
}

// TestRouteSyncer_HostnameOwnership_GRPCRoutesEnforced pins that the layer
// covers GRPCRoutes identically — both kinds claim hostnames.
func TestRouteSyncer_HostnameOwnership_GRPCRoutesEnforced(t *testing.T) {
	t.Parallel()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "team-b",
		Labels: map[string]string{ownershipLabelKey: "team-b.example.com"},
	}}
	gwNamespace := gatewayv1.Namespace("default")
	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-capture", Namespace: "team-b"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "test-gateway", Namespace: &gwNamespace}},
			},
			Hostnames: []gatewayv1.Hostname{"grpc.team-a.example.com"},
		},
	}

	syncer := newOwnershipSyncer(t, namespace, grpcRoute)

	result, err := syncer.getRelevantGRPCRoutes(context.Background(), nil)
	require.NoError(t, err)

	assert.Empty(t, result.accepted)
	require.Len(t, result.rejected, 1)
}
