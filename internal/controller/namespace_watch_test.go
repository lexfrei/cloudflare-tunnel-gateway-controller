package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestNamespaceScopedRequests pins the hostname-ownership reconvergence
// mapper: a Namespace label event must enqueue exactly the relevant routes of
// THAT namespace (any one of them triggers a full sync, which re-evaluates
// ownership for everything), and nothing from other namespaces.
func TestNamespaceScopedRequests(t *testing.T) {
	t.Parallel()

	all := func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "app"}},
			{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "api"}},
			{NamespacedName: types.NamespacedName{Namespace: "team-b", Name: "web"}},
		}
	}

	mapper := namespaceScopedRequests(all)

	requests := mapper(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
	})
	assert.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "app"}},
		{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "api"}},
	}, requests, "only the relabelled namespace's routes must be enqueued")

	assert.Empty(t, mapper(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-c"},
	}), "a namespace without relevant routes enqueues nothing")
}
