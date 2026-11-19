package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newScopedSecretCacheAndClient builds a REAL controller-runtime cache via
// cache.New, with the exact ByObject Secret scoping manager.go configures, and
// the production client on top of it: newSecretScopeClient — the manager's
// NewClient — reading through the cache. No API server is involved: a static
// RESTMapper stands in for discovery (Secret is namespaced), and the
// multi-namespace cache refuses an out-of-scope read before touching any
// informer — that refusal is the error under test. Because the error comes
// from the vendored cache itself, a controller-runtime upgrade that rewords it
// fails these tests, where hand-copied strings would keep passing.
func newScopedSecretCacheAndClient(t *testing.T, namespaces ...string) (cache.Cache, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	// The cache asks the mapper for the scope of the kind it is handed, so a
	// List read resolves "SecretList", not "Secret" — both must be mapped.
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("SecretList"), meta.RESTScopeNamespace)

	scoped := make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		scoped[namespace] = cache.Config{}
	}

	// Never contacted: the out-of-scope refusal happens before any informer
	// (or the HTTP client behind it) is used.
	config := &rest.Config{Host: "https://cache-scope.invalid"}

	scopedCache, err := cache.New(config, cache.Options{
		Scheme: scheme,
		Mapper: mapper,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: scoped},
		},
	})
	require.NoError(t, err)

	wrapped, err := newSecretScopeClient(config, client.Options{
		Scheme: scheme,
		Mapper: mapper,
		Cache:  &client.CacheOptions{Reader: scopedCache},
	})
	require.NoError(t, err)

	return scopedCache, wrapped
}

// TestCacheScopeErrorMarker_MatchesRealCacheError pins cacheScopeErrorMarker
// to the error the vendored multi-namespace cache actually returns for a read
// outside its namespace set, for both Get and List. If this fails after a
// controller-runtime bump, the cache reworded its out-of-scope error:
// update cacheScopeErrorMarker (secret_scope_client.go) to the new fixed
// fragment — and first check whether the new controller-runtime exposes a
// typed error for it, which would replace text matching entirely.
func TestCacheScopeErrorMarker_MatchesRealCacheError(t *testing.T) {
	t.Parallel()

	scopedCache, _ := newScopedSecretCacheAndClient(t, "listed")

	getErr := scopedCache.Get(context.Background(),
		types.NamespacedName{Namespace: "unlisted", Name: "edge-cert"}, &corev1.Secret{})
	require.Error(t, getErr)
	assert.Contains(t, getErr.Error(), cacheScopeErrorMarker,
		"the real cache's out-of-scope Get error no longer carries the marker; the wrapper would pass it through as transient and the partition would requeue forever")

	listErr := scopedCache.List(context.Background(), &corev1.SecretList{}, client.InNamespace("unlisted"))
	require.Error(t, listErr)
	assert.Contains(t, listErr.Error(), cacheScopeErrorMarker,
		"the real cache's out-of-scope List error no longer carries the marker; the wrapper would pass it through as transient and the partition would requeue forever")
}
