//go:build e2e

package e2e

// Unit test for the applyObject helper's delete-wait semantics: the helper
// must wait until the old object is actually gone (slow apiservers under
// load keep returning it for a while) instead of sleeping a fixed second
// and racing Create against an unfinished delete.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyObject_WaitsForSlowDeletion(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Data:       map[string]string{"old": "data"},
	}

	// Simulate a slow apiserver: after the Delete, Get keeps returning the
	// (deleted) object for the next few polls before admitting NotFound.
	var deleted atomic.Bool

	var getsAfterDelete atomic.Int32

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleted.Store(true)

				return cl.Delete(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if deleted.Load() && getsAfterDelete.Add(1) <= 3 {
					// Pretend the object is still there.
					if cm, ok := obj.(*corev1.ConfigMap); ok {
						existing.DeepCopyInto(cm)

						return nil
					}
				}

				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	replacement := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Data:       map[string]string{"new": "data"},
	}

	applyObject(context.Background(), t, cli, replacement)

	assert.GreaterOrEqual(t, getsAfterDelete.Load(), int32(3),
		"the helper must keep polling while the old object is still visible")

	var final corev1.ConfigMap
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "cm", Namespace: "ns"}, &final))
	assert.Equal(t, map[string]string{"new": "data"}, final.Data, "the replacement object must win")
}

func TestApplyObject_CreatesWhenAbsent(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Data:       map[string]string{"k": "v"},
	}

	applyObject(context.Background(), t, cli, obj)

	var got corev1.ConfigMap
	err := cli.Get(context.Background(), types.NamespacedName{Name: "cm", Namespace: "ns"}, &got)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "v"}, got.Data)
}
