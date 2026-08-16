package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// listenerTransitionFixtures returns the GatewayClass, GatewayClassConfig and
// credential Secret a Gateway needs to reconcile past config resolution.
func listenerTransitionFixtures() (*gatewayv1.GatewayClass, *v1alpha1.GatewayClassConfig, *corev1.Secret) {
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

	classConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-creds",
				Namespace: "default",
			},
			TunnelID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	}

	return gatewayClass, classConfig, secret
}

// gatewayWithSeededListenerStatus builds a single-HTTP-listener Gateway whose
// listener status already carries the given conditions, stamped at seededAt.
func gatewayWithSeededListenerStatus(seeded []metav1.Condition) *gatewayv1.Gateway {
	return gatewayWithPinnedListenerStatus(nil, seeded)
}

// gatewayWithPinnedListenerStatus is gatewayWithSeededListenerStatus with an
// optional listener hostname, which decides whether the reconcile emits the
// permissive-hostname advisory.
func gatewayWithPinnedListenerStatus(hostname *gatewayv1.Hostname, seeded []metav1.Condition) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostname},
			},
		},
		Status: gatewayv1.GatewayStatus{
			Listeners: []gatewayv1.ListenerStatus{
				{Name: "http", Conditions: seeded},
			},
		},
	}
}

func seededListenerCondition(condType string, status metav1.ConditionStatus, reason string, at metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: 1,
		LastTransitionTime: at,
		Reason:             reason,
		Message:            "seeded by a previous reconcile",
	}
}

// TestGatewayListenerConditions_PreserveLastTransitionTimeWhenUnchanged pins
// that a reconcile which reaches the same per-listener verdict leaves each
// condition's LastTransitionTime alone. Per the metav1.Condition contract the
// timestamp marks the last STATUS transition, so restamping it every reconcile
// makes status diffing and transition alerting see churn that never happened.
func TestGatewayListenerConditions_PreserveLastTransitionTimeWhenUnchanged(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))

	gateway := gatewayWithSeededListenerStatus([]metav1.Condition{
		seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
			metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt),
		seededListenerCondition(string(gatewayv1.ListenerConditionProgrammed),
			metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), seededAt),
		seededListenerCondition(string(gatewayv1.ListenerConditionResolvedRefs),
			metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), seededAt),
	})

	gatewayClass, classConfig, secret := listenerTransitionFixtures()
	fakeClient := setupGatewayFakeClient(gateway, classConfig, secret, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &updated))
	require.Len(t, updated.Status.Listeners, 1)

	for _, condType := range []string{
		string(gatewayv1.ListenerConditionAccepted),
		string(gatewayv1.ListenerConditionProgrammed),
		string(gatewayv1.ListenerConditionResolvedRefs),
	} {
		condition := findCondition(updated.Status.Listeners[0].Conditions, condType)
		require.NotNil(t, condition, "condition %s must be present", condType)
		assert.Equal(t, metav1.ConditionTrue, condition.Status, "condition %s must still be True", condType)
		assert.True(t, condition.LastTransitionTime.Equal(&seededAt),
			"condition %s did not transition, so its LastTransitionTime must be preserved", condType)
	}
}

// TestGatewayListenerConditions_UpdateLastTransitionTimeOnTransition is the
// other half: when a listener condition's Status genuinely flips, the timestamp
// MUST advance.
func TestGatewayListenerConditions_UpdateLastTransitionTimeOnTransition(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))

	gateway := gatewayWithSeededListenerStatus([]metav1.Condition{
		seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
			metav1.ConditionFalse, string(gatewayv1.ListenerReasonUnsupportedProtocol), seededAt),
	})

	gatewayClass, classConfig, secret := listenerTransitionFixtures()
	fakeClient := setupGatewayFakeClient(gateway, classConfig, secret, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &updated))
	require.Len(t, updated.Status.Listeners, 1)

	accepted := findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "an HTTP listener is servable, so it becomes Accepted")
	assert.True(t, accepted.LastTransitionTime.After(seededAt.Time),
		"a real status flip must advance LastTransitionTime")
}

// TestListenerSetEntryConditions_PreserveLastTransitionTimeWhenUnchanged pins
// the same contract on ListenerSet entry statuses, which are built by a
// separate code path.
func TestListenerSetEntryConditions_PreserveLastTransitionTimeWhenUnchanged(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	from := gatewayv1.NamespacesFromSame

	gatewayClass := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gateway.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 81, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
		Status: gatewayv1.ListenerSetStatus{
			Listeners: []gatewayv1.ListenerEntryStatus{
				{
					Name: "ls-l1",
					Conditions: []metav1.Condition{
						seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
							metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt),
						seededListenerCondition(string(gatewayv1.ListenerConditionProgrammed),
							metav1.ConditionTrue, string(gatewayv1.ListenerReasonProgrammed), seededAt),
					},
				},
			},
		},
	}

	reconciler, cli := newListenerSetReconciler(t, gatewayClass, gateway, listenerSet)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, listenerSet.Name, listenerSet.Namespace)
	require.Len(t, updated.Status.Listeners, 1)

	for _, condType := range []string{
		string(gatewayv1.ListenerConditionAccepted),
		string(gatewayv1.ListenerConditionProgrammed),
	} {
		condition := findCondition(updated.Status.Listeners[0].Conditions, condType)
		require.NotNil(t, condition, "condition %s must be present", condType)
		assert.Equal(t, metav1.ConditionTrue, condition.Status, "condition %s must still be True", condType)
		assert.True(t, condition.LastTransitionTime.Equal(&seededAt),
			"condition %s did not transition, so its LastTransitionTime must be preserved", condType)
	}
}

// TestListenerSetEntryConditions_UpdateLastTransitionTimeOnTransition covers a
// genuine entry-level flip.
func TestListenerSetEntryConditions_UpdateLastTransitionTimeOnTransition(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	from := gatewayv1.NamespacesFromSame

	gatewayClass := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gateway.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 81, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
		Status: gatewayv1.ListenerSetStatus{
			Listeners: []gatewayv1.ListenerEntryStatus{
				{
					Name: "ls-l1",
					Conditions: []metav1.Condition{
						seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
							metav1.ConditionFalse, string(gatewayv1.ListenerReasonUnsupportedProtocol), seededAt),
					},
				},
			},
		},
	}

	reconciler, cli := newListenerSetReconciler(t, gatewayClass, gateway, listenerSet)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, listenerSet.Name, listenerSet.Namespace)
	require.Len(t, updated.Status.Listeners, 1)

	accepted := findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
	assert.True(t, accepted.LastTransitionTime.After(seededAt.Time),
		"a real status flip must advance LastTransitionTime")
}

// TestGatewayListenerConditions_DropConditionNoLongerEmitted pins the third
// behaviour of the merge: a condition the reconcile stops emitting must
// disappear. The permissive-hostname advisory is the live case — it clears the
// moment a listener pins a hostname — and preserving timestamps by running
// meta.SetStatusCondition over the prior slice would strand it on the listener
// forever while every preserve/transition assertion still passed.
func TestGatewayListenerConditions_DropConditionNoLongerEmitted(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	pinned := gatewayv1.Hostname("app.example.com")

	// The advisory was raised while the listener was unpinned; the spec below
	// now pins a hostname, so this reconcile must not re-emit it.
	gateway := gatewayWithPinnedListenerStatus(&pinned, []metav1.Condition{
		seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
			metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt),
		seededListenerCondition(listenerConditionPermissiveHostname,
			metav1.ConditionTrue, listenerReasonUnpinnedHostnameAllowsAll, seededAt),
	})

	gatewayClass, classConfig, secret := listenerTransitionFixtures()
	fakeClient := setupGatewayFakeClient(gateway, classConfig, secret, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &updated))
	require.Len(t, updated.Status.Listeners, 1)

	require.NotNil(t, findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)),
		"the conditions this reconcile does emit must still be written")
	assert.Nil(t, findCondition(updated.Status.Listeners[0].Conditions, listenerConditionPermissiveHostname),
		"an advisory the reconcile no longer emits must be dropped, not carried forward")
}

// TestPreserveConditionTransitions_ForeignConditions pins the Gateway API
// clause that an implementation must not remove, reorder, or otherwise touch
// a listener condition type it is not responsible for.
func TestPreserveConditionTransitions_ForeignConditions(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	foreign := metav1.Condition{
		Type:               "special.io/SomeField",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 7,
		LastTransitionTime: seededAt,
		Reason:             "SomeReason",
		Message:            "set by a foreign controller",
	}
	foreignB := metav1.Condition{
		Type:               "other.io/AnotherField",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 3,
		LastTransitionTime: seededAt,
		Reason:             "OtherReason",
		Message:            "also set by a foreign controller",
	}
	own := seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
		metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt)

	tests := []struct {
		name    string
		prior   []metav1.Condition
		desired []metav1.Condition
		check   func(t *testing.T, merged []metav1.Condition)
	}{
		{
			name:    "foreign condition kept verbatim",
			prior:   []metav1.Condition{foreign},
			desired: []metav1.Condition{own},
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				got := findCondition(merged, foreign.Type)
				require.NotNil(t, got, "foreign condition must survive the merge")
				assert.Equal(t, foreign, *got, "foreign condition must be carried forward unchanged")
			},
		},
		{
			name:    "foreign order preserved with two foreign entries",
			prior:   []metav1.Condition{foreign, foreignB},
			desired: []metav1.Condition{own},
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				var foreignTypesInOrder []string

				for _, c := range merged {
					if c.Type == foreign.Type || c.Type == foreignB.Type {
						foreignTypesInOrder = append(foreignTypesInOrder, c.Type)
					}
				}

				assert.Equal(t, []string{foreign.Type, foreignB.Type}, foreignTypesInOrder,
					"foreign conditions must keep their relative order")
			},
		},
		{
			name:    "own condition absent from desired is still dropped",
			prior:   []metav1.Condition{own, foreign},
			desired: nil,
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				assert.Nil(t, findCondition(merged, own.Type),
					"a controller-owned type no longer emitted must be dropped")
				assert.NotNil(t, findCondition(merged, foreign.Type),
					"a foreign type must survive even when desired is empty")
			},
		},
		{
			name:    "foreign condition keeps its position ahead of own conditions",
			prior:   []metav1.Condition{foreign, own},
			desired: []metav1.Condition{own},
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				require.Len(t, merged, 2)
				assert.Equal(t, foreign.Type, merged[0].Type, "a foreign condition must not be moved behind the rebuilt own conditions")
				assert.Equal(t, own.Type, merged[1].Type)
			},
		},
		{
			name: "desired type outside the owned list is not duplicated",
			prior: []metav1.Condition{
				{Type: "cf.example/Future", Status: metav1.ConditionFalse, LastTransitionTime: seededAt, Reason: "Old"},
			},
			desired: []metav1.Condition{
				{Type: "cf.example/Future", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Now(), Reason: "New"},
			},
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				require.Len(t, merged, 1, "a type emitted this reconcile must appear once, whatever the ownership list says")
				assert.Equal(t, metav1.ConditionTrue, merged[0].Status)
			},
		},
		{
			name: "own condition present keeps LastTransitionTime when unchanged",
			prior: []metav1.Condition{
				seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
					metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt),
				foreign,
			},
			desired: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "recomputed this reconcile",
				},
			},
			check: func(t *testing.T, merged []metav1.Condition) {
				t.Helper()

				got := findCondition(merged, string(gatewayv1.ListenerConditionAccepted))
				require.NotNil(t, got)
				assert.True(t, got.LastTransitionTime.Equal(&seededAt),
					"status did not change, so LastTransitionTime must be preserved")

				fgot := findCondition(merged, foreign.Type)
				require.NotNil(t, fgot)
				assert.Equal(t, foreign, *fgot)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			merged := preserveConditionTransitions(tt.prior, tt.desired)
			tt.check(t, merged)
		})
	}
}

// TestGatewayListenerConditions_ForeignConditionSurvivesReconcile is the
// reconciler-level twin of TestPreserveConditionTransitions_ForeignConditions
// for the Gateway listener status path.
func TestGatewayListenerConditions_ForeignConditionSurvivesReconcile(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	// ObservedGeneration ahead of the Gateway's own generation: a foreign
	// condition must not trip the observedGeneration regression guard either.
	foreign := metav1.Condition{
		Type:               "special.io/SomeField",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 5,
		LastTransitionTime: seededAt,
		Reason:             "SomeReason",
		Message:            "set by a foreign controller",
	}

	gateway := gatewayWithSeededListenerStatus([]metav1.Condition{foreign})

	gatewayClass, classConfig, secret := listenerTransitionFixtures()
	fakeClient := setupGatewayFakeClient(gateway, classConfig, secret, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &updated))
	require.Len(t, updated.Status.Listeners, 1)

	got := findCondition(updated.Status.Listeners[0].Conditions, foreign.Type)
	require.NotNil(t, got, "a foreign listener condition must not be removed by the reconcile")
	assert.Equal(t, foreign, *got)
	require.NotNil(t, findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)),
		"the reconcile must still write its own listener conditions next to the foreign one")
}

// TestListenerSetEntryConditions_ForeignConditionSurvivesReconcile is the
// ListenerSet twin of TestGatewayListenerConditions_ForeignConditionSurvivesReconcile.
func TestListenerSetEntryConditions_ForeignConditionSurvivesReconcile(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	from := gatewayv1.NamespacesFromSame
	foreign := metav1.Condition{
		Type:               "special.io/SomeField",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 5,
		LastTransitionTime: seededAt,
		Reason:             "SomeReason",
		Message:            "set by a foreign controller",
	}

	gatewayClass := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gateway.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 81, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
		Status: gatewayv1.ListenerSetStatus{
			Listeners: []gatewayv1.ListenerEntryStatus{
				{Name: "ls-l1", Conditions: []metav1.Condition{foreign}},
			},
		},
	}

	reconciler, cli := newListenerSetReconciler(t, gatewayClass, gateway, listenerSet)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, listenerSet.Name, listenerSet.Namespace)
	require.Len(t, updated.Status.Listeners, 1)

	got := findCondition(updated.Status.Listeners[0].Conditions, foreign.Type)
	require.NotNil(t, got, "a foreign listener condition must not be removed by the reconcile")
	assert.Equal(t, foreign, *got)
	require.NotNil(t, findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)),
		"the reconcile must still write its own listener conditions next to the foreign one")
}

func TestOwnedListenerConditionsStale(t *testing.T) {
	t.Parallel()

	const reconciledGen = 3

	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{
			name:       "own condition newer than reconciled generation is stale",
			conditions: []metav1.Condition{{Type: string(gatewayv1.ListenerConditionAccepted), ObservedGeneration: reconciledGen + 1}},
			want:       true,
		},
		{
			name:       "own condition at reconciled generation is not stale",
			conditions: []metav1.Condition{{Type: string(gatewayv1.ListenerConditionAccepted), ObservedGeneration: reconciledGen}},
			want:       false,
		},
		{
			name:       "foreign condition newer than reconciled generation is ignored",
			conditions: []metav1.Condition{{Type: "special.io/SomeField", ObservedGeneration: reconciledGen + 10}},
			want:       false,
		},
		{
			name:       "own advisory condition newer than reconciled generation is stale",
			conditions: []metav1.Condition{{Type: listenerConditionPermissiveHostname, ObservedGeneration: reconciledGen + 1}},
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, ownedListenerConditionsStale(reconciledGen, tt.conditions))
		})
	}
}

// TestListenerSetEntryConditions_DropConditionNoLongerEmitted is the
// ListenerSet twin of the drop pin.
func TestListenerSetEntryConditions_DropConditionNoLongerEmitted(t *testing.T) {
	t.Parallel()

	seededAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	from := gatewayv1.NamespacesFromSame
	pinned := gatewayv1.Hostname("tenant.example.com")

	gatewayClass := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gateway.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 81, Protocol: gatewayv1.HTTPProtocolType, Hostname: &pinned},
			},
		},
		Status: gatewayv1.ListenerSetStatus{
			Listeners: []gatewayv1.ListenerEntryStatus{
				{
					Name: "ls-l1",
					Conditions: []metav1.Condition{
						seededListenerCondition(string(gatewayv1.ListenerConditionAccepted),
							metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), seededAt),
						seededListenerCondition(listenerConditionPermissiveHostname,
							metav1.ConditionTrue, listenerReasonUnpinnedHostnameAllowsAll, seededAt),
					},
				},
			},
		},
	}

	reconciler, cli := newListenerSetReconciler(t, gatewayClass, gateway, listenerSet)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, listenerSet.Name, listenerSet.Namespace)
	require.Len(t, updated.Status.Listeners, 1)

	require.NotNil(t, findCondition(updated.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionAccepted)),
		"the conditions this reconcile does emit must still be written")
	assert.Nil(t, findCondition(updated.Status.Listeners[0].Conditions, listenerConditionPermissiveHostname),
		"an advisory the reconcile no longer emits must be dropped, not carried forward")
}
