package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const btlsTestController = "github.com/lexfrei/test"

func newBTLSStatusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := newBackendTLSPolicyScheme(t)
	require.NoError(t, gatewayv1beta1.Install(scheme))

	return scheme
}

func reconcileBTLSPolicy(t *testing.T, objects ...client.Object) *gatewayv1.BackendTLSPolicy {
	t.Helper()

	return reconcileBTLSPolicyWith(t, nil, objects...)
}

func reconcileBTLSPolicyWith(t *testing.T, recorder events.EventRecorder, objects ...client.Object) *gatewayv1.BackendTLSPolicy {
	t.Helper()

	scheme := newBTLSStatusScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&gatewayv1.BackendTLSPolicy{}).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: btlsTestController, Recorder: recorder}

	key := types.NamespacedName{Namespace: "ns", Name: "p"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), key, &refreshed))

	return &refreshed
}

func computeConditionsWith(t *testing.T, policy *gatewayv1.BackendTLSPolicy, objects ...client.Object) []metav1.Condition {
	t.Helper()

	scheme := newBTLSStatusScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, policy)...).Build()

	return mustComputeConditions(t, &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme}, policy)
}

func ancestorNames(policy *gatewayv1.BackendTLSPolicy) []string {
	names := make([]string, 0, len(policy.Status.Ancestors))
	for _, ancestor := range policy.Status.Ancestors {
		names = append(names, string(*ancestor.AncestorRef.Namespace)+"/"+string(ancestor.AncestorRef.Name))
	}

	return names
}

// A targetRef names a core Service only when both group and kind say so; a
// same-named target in another group must not attach TLS to the Service.
func TestPolicyTargetsServicePort_RequiresCoreGroup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		group    string
		expected bool
	}{
		{"", true},
		{"core", true},
		{"example.com", false},
		{proxy.ServiceImportGroup, false},
	} {
		t.Run(tc.group, func(t *testing.T) {
			t.Parallel()

			policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
			policy.Spec.TargetRefs[0].Group = gatewayv1.Group(tc.group)

			targets, _, err := policyTargetsServicePort(policy, "svc", alwaysEmptyPortName)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, targets)
		})
	}
}

func TestReconcile_ForeignGroupTargetDoesNotClaimCoreService(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.TargetRefs[0].Group = "example.com"

	refreshed := reconcileBTLSPolicy(t,
		gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"),
		acceptedHTTPRoute(httpRouteFor("ns", "r", "gw", "svc"), btlsTestController), caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), policy)

	assert.Empty(t, refreshed.Status.Ancestors)
}

// A policy whose target is a backend kind this controller does not attach
// BackendTLSPolicy to is reported under the Gateways of the routes using that
// backend, instead of being ignored without a trace.
func TestReconcile_UnsupportedTargetKindReportsTargetNotFound(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "imported", "cm", time.Time{})
	policy.Spec.TargetRefs[0].Group = proxy.ServiceImportGroup
	policy.Spec.TargetRefs[0].Kind = proxy.ServiceImportKind

	route := acceptedHTTPRoute(httpRouteFor("ns", "r", "gw", "imported"), btlsTestController)
	route.Spec.Rules[0].BackendRefs[0].Group = new(gatewayv1.Group(proxy.ServiceImportGroup))
	route.Spec.Rules[0].BackendRefs[0].Kind = new(gatewayv1.Kind(proxy.ServiceImportKind))

	refreshed := reconcileBTLSPolicy(t,
		gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"),
		route, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), policy)

	require.Len(t, refreshed.Status.Ancestors, 1)

	accepted := meta.FindStatusCondition(refreshed.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.PolicyReasonTargetNotFound), accepted.Reason)
	assert.Contains(t, accepted.Message, "ServiceImport")
}

func TestComputeConditions_ForeignGroupPeerDoesNotConflict(t *testing.T) {
	t.Parallel()

	older := backendTLSPolicyFor("ns", "older", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	older.Spec.TargetRefs[0].Group = "example.com"
	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	conditions := computeConditionsWith(t, policy, older, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)))

	accepted := meta.FindStatusCondition(conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
}

// A ServiceImport (or any non-Service) backend never picks up the policy of a
// same-named core Service.
func TestBackendTLSResolver_NonServiceBackendGetsNoPolicy(t *testing.T) {
	t.Parallel()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}), caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))).
		Build()

	got, err := newBackendTLSResolver(fakeClient)(context.Background(), "ns", "svc", 443, false)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestReconcile_ListenerSetParentListsItsGateway(t *testing.T) {
	t.Parallel()

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ls"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"}},
	}

	route := httpRouteFor("ns", "r", "ls", "svc")
	route.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))
	route = acceptedHTTPRoute(route, btlsTestController)

	refreshed := reconcileBTLSPolicy(t,
		gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"), listenerSet,
		route, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}))

	assert.Equal(t, []string{"ns/gw"}, ancestorNames(refreshed))
}

func crossNamespaceRoute() *gatewayv1.HTTPRoute {
	route := acceptedHTTPRoute(httpRouteFor("route-ns", "r", "gw", "svc"), btlsTestController)
	route.Spec.Rules[0].BackendRefs[0].Namespace = new(gatewayv1.Namespace("ns"))

	return route
}

func serviceGrant(fromNamespace string) *gatewayv1beta1.ReferenceGrant {
	return &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "allow"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{
				Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(fromNamespace),
			}},
			To: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: serviceKind}},
		},
	}
}

func TestReconcile_GrantedCrossNamespaceRouteListsItsGateway(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		grant    *gatewayv1beta1.ReferenceGrant
		expected []string
	}{
		{name: "granted", grant: serviceGrant("route-ns"), expected: []string{"route-ns/gw"}},
		{name: "not granted", grant: serviceGrant("elsewhere"), expected: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			refreshed := reconcileBTLSPolicy(t,
				gatewayClassFor("cf-class", btlsTestController), gatewayFor("route-ns", "gw", "cf-class"),
				crossNamespaceRoute(), tc.grant, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)),
				backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}))

			assert.Equal(t, tc.expected, ancestorNames(refreshed))
		})
	}
}

func TestPoliciesForRouteChange_CrossNamespaceBackendEnqueuesPolicy(t *testing.T) {
	t.Parallel()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBTLSStatusScheme(t)).
		WithObjects(backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, ControllerName: btlsTestController}

	requests := r.policiesForRouteChange(context.Background(), crossNamespaceRoute())

	require.Len(t, requests, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "p"}, requests[0].NamespacedName)
}

func TestPoliciesForReferenceGrantChange_EnqueuesPoliciesInItsNamespace(t *testing.T) {
	t.Parallel()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBTLSStatusScheme(t)).
		WithObjects(
			backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
			backendTLSPolicyFor("other", "q", "svc", "cm", time.Time{}),
		).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, ControllerName: btlsTestController}

	requests := r.policiesForReferenceGrantChange(context.Background(), serviceGrant("route-ns"))

	require.Len(t, requests, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "p"}, requests[0].NamespacedName)
}

// Once no managed Gateway is affected, this controller's ancestor entries go,
// and other controllers' entries stay.
func TestReconcile_PrunesOwnAncestorsWhenNoGatewayRemains(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	foreign := gatewayv1.PolicyAncestorStatus{
		AncestorRef:    gatewayv1.ParentReference{Name: "their-gw"},
		ControllerName: "example.com/other",
		Conditions:     []metav1.Condition{{Type: string(gatewayv1.PolicyConditionAccepted), Status: metav1.ConditionTrue}},
	}
	ours := gatewayv1.PolicyAncestorStatus{
		AncestorRef:    gatewayAncestorRef(gatewayFor("ns", "gone", "cf-class")),
		ControllerName: btlsTestController,
		Conditions:     acceptedConditions(policy.Generation),
	}
	policy.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{foreign, ours}

	refreshed := reconcileBTLSPolicy(t, policy)

	require.Len(t, refreshed.Status.Ancestors, 1)
	assert.Equal(t, gatewayv1.GatewayController("example.com/other"), refreshed.Status.Ancestors[0].ControllerName)
}

// Past the 16-entry cap, each Gateway left out of status is told that the
// policy cannot be represented for it.
func TestReconcile_AncestorsOverCapSignalDroppedGateways(t *testing.T) {
	t.Parallel()

	objects := make([]client.Object, 0, 3+2*(policyAncestorStatusMaxCount+2))
	objects = append(objects,
		gatewayClassFor("cf-class", btlsTestController),
		caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)),
		backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
	)

	for idx := range policyAncestorStatusMaxCount + 2 {
		name := "gw-" + zeroPadded(idx)
		objects = append(objects, gatewayFor("ns", name, "cf-class"), acceptedHTTPRoute(httpRouteFor("ns", "r-"+name, name, "svc"), btlsTestController))
	}

	recorder := events.NewFakeRecorder(10)
	refreshed := reconcileBTLSPolicyWith(t, recorder, objects...)

	require.Len(t, refreshed.Status.Ancestors, policyAncestorStatusMaxCount)
	require.Len(t, recorder.Events, 2)

	for range 2 {
		event := <-recorder.Events
		assert.True(t, strings.HasPrefix(event, corev1.EventTypeWarning+" "), event)
		assert.Contains(t, event, "ns/p")
	}
}

func TestComputeConditions_SomeInvalidCARefsKeepPolicyAccepted(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.Validation.CACertificateRefs = append(policy.Spec.Validation.CACertificateRefs,
		gatewayv1.LocalObjectReference{Kind: configMapKind, Name: "missing"})

	conditions := computeConditionsWith(t, policy, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)))

	accepted := meta.FindStatusCondition(conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)

	resolved := meta.FindStatusCondition(conditions, string(gatewayv1.BackendTLSPolicyConditionResolvedRefs))
	require.NotNil(t, resolved)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef), resolved.Reason)
	assert.Contains(t, resolved.Message, "missing")
}

func TestBackendTLSResolver_SomeInvalidCARefsUseTheValidOnes(t *testing.T) {
	t.Parallel()

	caPEM := generateSelfSignedCAPEM(t)
	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.Validation.CACertificateRefs = append(policy.Spec.Validation.CACertificateRefs,
		gatewayv1.LocalObjectReference{Kind: configMapKind, Name: "missing"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(policy, caConfigMap("ns", "cm", caPEM)).
		Build()

	got, err := newBackendTLSResolver(fakeClient)(context.Background(), "ns", "svc", 443, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.Unenforceable)
	assert.Equal(t, caPEM, got.CABundlePEM)
}

// A policy that affects no Gateway and has no entry of ours is not written
// on every reconcile.
func TestReconcile_NoAncestorsAndNoOwnEntriesWritesNothing(t *testing.T) {
	t.Parallel()

	scheme := newBTLSStatusScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})).
		WithStatusSubresource(&gatewayv1.BackendTLSPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return errSimulatedCacheMiss
			},
		}).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: btlsTestController}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}})
	require.NoError(t, err)
}

// A parent that cannot be read is not proof that the Gateway stopped being an
// ancestor: the reconcile fails and retries, and the existing entry stays.
func TestReconcile_UnreadableParentKeepsAncestorsAndRetries(t *testing.T) {
	t.Parallel()

	listenerSetRoute := httpRouteFor("ns", "r", "ls", "svc")
	listenerSetRoute.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))
	listenerSetRoute = acceptedHTTPRoute(listenerSetRoute, btlsTestController)

	for _, tc := range []struct {
		name  string
		route *gatewayv1.HTTPRoute
		fails func(client.Object) bool
	}{
		{
			name: "gateway", route: acceptedHTTPRoute(httpRouteFor("ns", "r", "gw", "svc"), btlsTestController),
			fails: func(obj client.Object) bool { _, ok := obj.(*gatewayv1.Gateway); return ok },
		},
		{
			name: "gatewayclass", route: acceptedHTTPRoute(httpRouteFor("ns", "r", "gw", "svc"), btlsTestController),
			fails: func(obj client.Object) bool { _, ok := obj.(*gatewayv1.GatewayClass); return ok },
		},
		{
			name: "listenerset", route: listenerSetRoute,
			fails: func(obj client.Object) bool { _, ok := obj.(*gatewayv1.ListenerSet); return ok },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
			policy.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{{
				AncestorRef:    gatewayAncestorRef(gatewayFor("ns", "gw", "cf-class")),
				ControllerName: btlsTestController,
				Conditions:     acceptedConditions(policy.Generation),
			}}

			scheme := newBTLSStatusScheme(t)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"),
					tc.route, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), policy).
				WithStatusSubresource(&gatewayv1.BackendTLSPolicy{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if tc.fails(obj) {
							return errSimulatedCacheMiss
						}

						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: btlsTestController}

			key := types.NamespacedName{Namespace: "ns", Name: "p"}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.ErrorIs(t, err, errSimulatedCacheMiss)

			var refreshed gatewayv1.BackendTLSPolicy
			require.NoError(t, fakeClient.Get(context.Background(), key, &refreshed))
			assert.Len(t, refreshed.Status.Ancestors, 1)
		})
	}
}

// A Gateway reaching both a supported and an unsupported target of the policy
// is governed by the supported one.
func TestReconcile_SupportedTargetOutranksUnsupportedOnSameGateway(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.TargetRefs = append(policy.Spec.TargetRefs, gatewayv1.LocalPolicyTargetReferenceWithSectionName{
		LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
			Group: proxy.ServiceImportGroup, Kind: proxy.ServiceImportKind, Name: "imported",
		},
	})

	importRoute := acceptedHTTPRoute(httpRouteFor("ns", "z-import", "gw", "imported"), btlsTestController)
	importRoute.Spec.Rules[0].BackendRefs[0].Group = new(gatewayv1.Group(proxy.ServiceImportGroup))
	importRoute.Spec.Rules[0].BackendRefs[0].Kind = new(gatewayv1.Kind(proxy.ServiceImportKind))

	refreshed := reconcileBTLSPolicy(t,
		gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"),
		importRoute, acceptedHTTPRoute(httpRouteFor("ns", "a-svc", "gw", "svc"), btlsTestController),
		caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), policy)

	require.Len(t, refreshed.Status.Ancestors, 1)

	accepted := meta.FindStatusCondition(refreshed.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
}

func TestComputeConditions_SomeInvalidCARefsOnConflictedPolicy(t *testing.T) {
	t.Parallel()

	older := backendTLSPolicyFor("ns", "older", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	policy.Spec.Validation.CACertificateRefs = append(policy.Spec.Validation.CACertificateRefs,
		gatewayv1.LocalObjectReference{Kind: configMapKind, Name: "missing"})

	conditions := computeConditionsWith(t, policy, older, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)))

	accepted := meta.FindStatusCondition(conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, string(gatewayv1.PolicyReasonConflicted), accepted.Reason)

	resolved := meta.FindStatusCondition(conditions, string(gatewayv1.BackendTLSPolicyConditionResolvedRefs))
	require.NotNil(t, resolved)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef), resolved.Reason)
}

// routeParentStatuses stamps a status entry per parentRef of the route,
// written by controllerName with the given Accepted status.
func routeParentStatuses(
	parentRefs []gatewayv1.ParentReference, routeNamespace, controllerName string, accepted metav1.ConditionStatus,
) []gatewayv1.RouteParentStatus {
	statuses := make([]gatewayv1.RouteParentStatus, 0, len(parentRefs))
	for _, ref := range parentRefs {
		statuses = append(statuses, gatewayv1.RouteParentStatus{
			ParentRef:      statusParentRef(ref, routeNamespace),
			ControllerName: gatewayv1.GatewayController(controllerName),
			Conditions: []metav1.Condition{{
				Type: string(gatewayv1.RouteConditionAccepted), Status: accepted, Reason: "Test",
			}},
		})
	}

	return statuses
}

// acceptedHTTPRoute marks every parentRef of the route as accepted by
// controllerName, the state a route reaches once this controller binds it.
func acceptedHTTPRoute(route *gatewayv1.HTTPRoute, controllerName string) *gatewayv1.HTTPRoute {
	route.Status.Parents = routeParentStatuses(route.Spec.ParentRefs, route.Namespace, controllerName, metav1.ConditionTrue)

	return route
}

func acceptedGRPCRoute(route *gatewayv1.GRPCRoute, controllerName string) *gatewayv1.GRPCRoute {
	route.Status.Parents = routeParentStatuses(route.Spec.ParentRefs, route.Namespace, controllerName, metav1.ConditionTrue)

	return route
}

// A Gateway or ListenerSet that did not accept the route carries none of its
// traffic, so it is not an ancestor of the policy on the route's backend.
func TestReconcile_OnlyParentsThatAcceptedTheRouteAreAncestors(t *testing.T) {
	t.Parallel()

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ls"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"}},
	}

	for _, tc := range []struct {
		name       string
		listener   bool
		controller string
		accepted   metav1.ConditionStatus
		expected   []string
	}{
		{name: "gateway accepted", controller: btlsTestController, accepted: metav1.ConditionTrue, expected: []string{"ns/gw"}},
		{name: "gateway rejected", controller: btlsTestController, accepted: metav1.ConditionFalse, expected: []string{}},
		{name: "accepted by another controller", controller: "example.com/other", accepted: metav1.ConditionTrue, expected: []string{}},
		{name: "listenerset rejected", listener: true, controller: btlsTestController, accepted: metav1.ConditionFalse, expected: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			route := httpRouteFor("ns", "r", "gw", "svc")
			if tc.listener {
				route = httpRouteFor("ns", "r", "ls", "svc")
				route.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))
			}

			route.Status.Parents = routeParentStatuses(route.Spec.ParentRefs, route.Namespace, tc.controller, tc.accepted)

			refreshed := reconcileBTLSPolicy(t,
				gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw", "cf-class"), listenerSet.DeepCopy(),
				route, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}))

			assert.Equal(t, tc.expected, ancestorNames(refreshed))
		})
	}
}

func TestReconcile_AcceptanceIsCheckedPerParent(t *testing.T) {
	t.Parallel()

	route := httpRouteFor("ns", "r", "gw-a", "svc")
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: "gw-b"})
	route.Status.Parents = append(
		routeParentStatuses(route.Spec.ParentRefs[:1], route.Namespace, btlsTestController, metav1.ConditionFalse),
		routeParentStatuses(route.Spec.ParentRefs[1:], route.Namespace, btlsTestController, metav1.ConditionTrue)...)

	refreshed := reconcileBTLSPolicy(t,
		gatewayClassFor("cf-class", btlsTestController), gatewayFor("ns", "gw-a", "cf-class"), gatewayFor("ns", "gw-b", "cf-class"),
		route, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)), backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}))

	assert.Equal(t, []string{"ns/gw-b"}, ancestorNames(refreshed))
}

// Moving a ListenerSet to another Gateway leaves the route's status as it
// was, so the ListenerSet change itself must re-evaluate the policies on the
// backends of its routes.
func TestPoliciesForListenerSetChange_EnqueuesPoliciesOfItsRoutes(t *testing.T) {
	t.Parallel()

	attached := httpRouteFor("ns", "r", "ls", "svc")
	attached.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))
	grpcAttached := grpcRouteFor("ns", "g", "ls", "grpc-svc")
	grpcAttached.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBTLSStatusScheme(t)).
		WithObjects(
			attached, grpcAttached, httpRouteFor("ns", "elsewhere", "gw", "other-svc"),
			backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
			backendTLSPolicyFor("ns", "q", "grpc-svc", "cm", time.Time{}),
			backendTLSPolicyFor("ns", "unrelated", "other-svc", "cm", time.Time{}),
		).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, ControllerName: btlsTestController}

	listenerSet := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ls"}}
	requests := r.policiesForListenerSetChange(context.Background(), listenerSet)

	names := make([]string, 0, len(requests))
	for _, request := range requests {
		names = append(names, request.Name)
	}

	assert.ElementsMatch(t, []string{"p", "q"}, names)
}
