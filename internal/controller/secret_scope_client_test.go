package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// errAPIServerDown is an unrelated failure for the pass-through case. The
// out-of-scope errors themselves are NOT hand-copied here: the tests below
// read through a real controller-runtime cache (newScopedSecretCacheAndClient,
// secret_scope_client_realcache_test.go), so a controller-runtime upgrade that
// rewords the cache's error breaks them instead of passing silently.
var errAPIServerDown = errors.New("the apiserver is on fire")

// TestSecretScopeClient_OutOfScopeReadIsNotFound pins the error contract the
// scoped Secret cache needs: the multi-namespace cache reports a
// read outside its namespace set as a PLAIN error, which every Secret
// consumer classifies as transient — a single Gateway referencing an
// out-of-scope Secret would then stall its whole partition's config push and
// requeue forever. The wrapper turns it into a deterministic NotFound, so the
// reference is refused and isolated exactly like a missing Secret, with the
// remedy in the message. The read goes through a real scoped cache built with
// cache.New, exactly as the manager wires it.
func TestSecretScopeClient_OutOfScopeReadIsNotFound(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Namespace: "unlisted", Name: "edge-cert"}

	_, wrapped := newScopedSecretCacheAndClient(t, "listed")

	err := wrapped.Get(context.Background(), key, &corev1.Secret{})
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err),
		"an out-of-scope read must be a deterministic NotFound, not a transient error")
	assert.Contains(t, err.Error(), "unlisted", "the message must name the out-of-scope namespace")
	assert.Contains(t, err.Error(), "extraSecretNamespaces", "the message must carry the remedy")
}

// TestSecretScopeClient_OtherErrorsAndHitsPassThrough pins the wrapper's
// transparency: a real read works, a NotFound stays a NotFound, and an
// unrelated failure keeps its transient classification.
func TestSecretScopeClient_OtherErrorsAndHitsPassThrough(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "listed"}}
	inner := fake.NewClientBuilder().WithObjects(secret).Build()
	wrapped := &secretScopeClient{Client: inner}

	var got corev1.Secret
	require.NoError(t, wrapped.Get(context.Background(), types.NamespacedName{Namespace: "listed", Name: "tok"}, &got))

	err := wrapped.Get(context.Background(), types.NamespacedName{Namespace: "listed", Name: "absent"}, &got)
	assert.True(t, apierrors.IsNotFound(err))

	boom := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errAPIServerDown
		},
	}).Build()
	wrapped = &secretScopeClient{Client: boom}

	err = wrapped.Get(context.Background(), types.NamespacedName{Namespace: "listed", Name: "tok"}, &got)
	require.Error(t, err)
	assert.False(t, apierrors.IsNotFound(err), "a genuine failure must stay transient")
}

// TestSecretScopeClient_ListConversion pins the same contract for List, which
// the real multi-namespace cache refuses with the sibling message.
func TestSecretScopeClient_ListConversion(t *testing.T) {
	t.Parallel()

	_, wrapped := newScopedSecretCacheAndClient(t, "listed")

	err := wrapped.List(context.Background(), &corev1.SecretList{}, client.InNamespace("unlisted"))
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}
