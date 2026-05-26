//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestDeleteAllRoutes_ScopedToSubtest is the regression guard for
// issue #265. It builds three HTTPRoutes in the same namespace:
// one labelled for the running subtest, one labelled for a sibling
// subtest, and one with no owner label (the "legacy" shape that
// predates the labelling). It then calls deleteAllRoutes inside the
// running subtest's t. Expectation: only the route owned by THIS
// subtest disappears; the sibling and the legacy route both survive.
//
// The bug the issue documents was that deleteAllRoutes was a blanket
// namespace wipe -- a passing sibling's defer would happily delete
// the failing subtest's retained routes. This test fails closed if
// that wipe ever returns, regardless of whether someone later edits
// the helper to "be smarter" by ignoring labels.
//
// Uses sigs.k8s.io/controller-runtime/pkg/client/fake so it does NOT
// need a live cluster and runs under any tag. The build tag stays
// e2e because all the helpers under test live there.
func TestDeleteAllRoutes_ScopedToSubtest(t *testing.T) {
	t.Parallel()

	const (
		ns           = "e2e-scope-test"
		siblingName  = "TestDeleteAllRoutes_ScopedToSubtest_SiblingFixture"
		thisRoute    = "route-this-subtest"
		siblingRoute = "route-sibling-subtest"
		legacyRoute  = "route-legacy-no-owner"
	)

	// Per-test scheme (NOT scheme.Scheme): the global is shared
	// across the package and t.Parallel() callers, so installing
	// gatewayv1 into it races with sibling tests that do the same.
	s := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(s))

	t.Run("subtest", func(t *testing.T) {
		t.Parallel()

		// Build three routes in the same namespace.
		fakeClient := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(
				&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
					Name:      thisRoute,
					Namespace: ns,
					Labels:    map[string]string{ownerLabelKey: subtestLabelValue(t.Name())},
				}},
				&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
					Name:      siblingRoute,
					Namespace: ns,
					Labels:    map[string]string{ownerLabelKey: subtestLabelValue(siblingName)},
				}},
				&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
					Name:      legacyRoute,
					Namespace: ns,
					// No owner label -- represents resources created before this
					// PR landed, or by a future test that forgets to use the
					// helpers. Per the scoping contract, deleteAllRoutes MUST
					// leave them alone.
				}},
			).
			Build()

		cfg := testConfig{TestNamespace: ns}

		deleteAllRoutes(t, fakeClient, cfg)

		// Re-list to see what's left.
		var remaining gatewayv1.HTTPRouteList
		require.NoError(t, fakeClient.List(context.Background(), &remaining, client.InNamespace(ns)))

		names := make(map[string]struct{}, len(remaining.Items))
		for _, r := range remaining.Items {
			names[r.Name] = struct{}{}
		}

		assert.NotContains(t, names, thisRoute,
			"deleteAllRoutes must delete the running subtest's own route")
		assert.Contains(t, names, siblingRoute,
			"deleteAllRoutes must NOT delete a sibling subtest's route (#265 regression)")
		assert.Contains(t, names, legacyRoute,
			"deleteAllRoutes must NOT delete a route with no owner label (legacy / external)")
	})
}

// TestWipeAllRoutesInNamespace_BlanketWipe pins the contract that the
// top-level "clean slate" helper still wipes everything in the
// namespace -- that's what we want for the pre-test sweep before any
// subtest has started. If a future refactor accidentally narrows
// wipeAllRoutesInNamespace to a subtest-scoped filter, prior-run
// leftovers would survive and pollute the first subtest's
// expectations.
func TestWipeAllRoutesInNamespace_BlanketWipe(t *testing.T) {
	t.Parallel()

	const ns = "e2e-wipe-test"

	// Per-test scheme (NOT scheme.Scheme): the global is shared
	// across the package and t.Parallel() callers, so installing
	// gatewayv1 into it races with sibling tests that do the same.
	s := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(s))

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
				Name:      "route-owned-by-subtest-a",
				Namespace: ns,
				Labels:    map[string]string{ownerLabelKey: subtestLabelValue("TestSomething/SubA")},
			}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
				Name:      "route-owned-by-subtest-b",
				Namespace: ns,
				Labels:    map[string]string{ownerLabelKey: subtestLabelValue("TestSomething/SubB")},
			}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
				Name:      "route-no-owner",
				Namespace: ns,
			}},
		).
		Build()

	cfg := testConfig{TestNamespace: ns}

	wipeAllRoutesInNamespace(t, fakeClient, cfg)

	var remaining gatewayv1.HTTPRouteList
	require.NoError(t, fakeClient.List(context.Background(), &remaining, client.InNamespace(ns)))

	assert.Empty(t, remaining.Items,
		"wipeAllRoutesInNamespace must delete every HTTPRoute in the namespace, regardless of owner")
}
