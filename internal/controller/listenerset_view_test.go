package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

// listSpyClient counts List(ListenerSetList) calls so tests can assert how
// often the merge view re-lists.
type listSpyClient struct {
	client.Client

	listenerSetLists atomic.Int64
}

func (c *listSpyClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*gatewayv1.ListenerSetList); ok {
		c.listenerSetLists.Add(1)
	}

	return errors.Wrap(c.Client.List(ctx, list, opts...), "list")
}

func viewTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	return scheme
}

func viewGateway(from gatewayv1.FromNamespaces) *gatewayv1.Gateway {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra", Generation: 1},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "managed-class"},
	}

	gw.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
	}

	return gw
}

func viewListenerSet(name string, host gatewayv1.Hostname, port gatewayv1.PortNumber, proto gatewayv1.ProtocolType) *gatewayv1.ListenerSet {
	return &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "l1", Port: port, Protocol: proto, Hostname: &host},
			},
		},
	}
}

// TestListenerViewCache_ForGateway_EquivalentToDirectMerge pins behaviour: the
// view the cache hands out is byte-for-byte the merge a consumer would have
// built directly. This is the safety net for the whole refactor — if the cache
// ever diverges from listenermerge.Merge(gw, collectAccepted(gw)), this fails.
func TestListenerViewCache_ForGateway_EquivalentToDirectMerge(t *testing.T) {
	t.Parallel()

	hostA := gatewayv1.Hostname("a.example.com")
	hostB := gatewayv1.Hostname("b.example.com")

	tests := []struct {
		name string
		from gatewayv1.FromNamespaces
		sets []*gatewayv1.ListenerSet
	}{
		{name: "no listenersets", from: gatewayv1.NamespacesFromSame},
		{
			name: "conflict-free",
			from: gatewayv1.NamespacesFromSame,
			sets: []*gatewayv1.ListenerSet{
				viewListenerSet("ls-a", hostA, 80, gatewayv1.HTTPProtocolType),
				viewListenerSet("ls-b", hostB, 80, gatewayv1.HTTPProtocolType),
			},
		},
		{
			name: "hostname conflict",
			from: gatewayv1.NamespacesFromAll,
			sets: []*gatewayv1.ListenerSet{
				viewListenerSet("ls-a", hostA, 80, gatewayv1.HTTPProtocolType),
				viewListenerSet("ls-b", hostA, 80, gatewayv1.HTTPProtocolType),
			},
		},
		{
			name: "protocol conflict",
			from: gatewayv1.NamespacesFromAll,
			sets: []*gatewayv1.ListenerSet{
				viewListenerSet("ls-a", hostA, 80, gatewayv1.HTTPProtocolType),
				viewListenerSet("ls-b", hostB, 80, gatewayv1.HTTPSProtocolType),
			},
		},
		{name: "from none rejects all", from: gatewayv1.NamespacesFromNone, sets: []*gatewayv1.ListenerSet{
			viewListenerSet("ls-a", hostA, 80, gatewayv1.HTTPProtocolType),
		}},
		{name: "from selector bypasses store", from: gatewayv1.NamespacesFromSelector, sets: []*gatewayv1.ListenerSet{
			viewListenerSet("ls-a", hostA, 80, gatewayv1.HTTPProtocolType),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gw := viewGateway(tt.from)

			objs := []client.Object{gw}
			for _, ls := range tt.sets {
				objs = append(objs, ls)
			}

			cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(objs...).Build()

			want, err := collectAcceptedListenerSetsForGateway(context.Background(), cli, gw)
			require.NoError(t, err)
			wantMerged := listenermerge.Merge(gw, want)

			cache := newListenerViewCache(cli, newMergeViewStore())
			view, err := cache.forGateway(context.Background(), gw)
			require.NoError(t, err)

			assert.Equal(t, wantMerged, view.merged)
			assert.Equal(t, want, view.acceptedSets)
		})
	}
}

// TestMergeViewStore_ReusesUnchanged proves the store skips the Merge on a
// fingerprint hit: two resolves over unchanged inputs return the SAME view
// pointer (a recompute would allocate a fresh one).
func TestMergeViewStore_ReusesUnchanged(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSame)
	ls := viewListenerSet("ls-a", "a.example.com", 80, gatewayv1.HTTPProtocolType)
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, ls).Build()

	store := newMergeViewStore()

	first, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	second, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	assert.Same(t, first, second, "unchanged inputs must reuse the cached view, not rebuild it")
}

// TestMergeViewStore_RecomputesOnGenerationBump proves a sibling ListenerSet
// spec change (generation bump) invalidates the entry — the reason the
// fingerprint must cover ListenerSet generation, not just the Gateway's.
func TestMergeViewStore_RecomputesOnGenerationBump(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSame)
	ls := viewListenerSet("ls-a", "a.example.com", 80, gatewayv1.HTTPProtocolType)
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, ls).Build()

	store := newMergeViewStore()

	first, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	// Bump the ListenerSet's generation in the backing store (spec edit).
	var live gatewayv1.ListenerSet
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(ls), &live))
	live.Generation = 2
	require.NoError(t, cli.Update(context.Background(), &live))

	second, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	assert.NotSame(t, first, second, "a ListenerSet generation bump must force a rebuild")
}

// TestMergeViewStore_SelectorBypassesCache proves From=Selector Gateways never
// populate the store (namespace labels are outside the fingerprint).
func TestMergeViewStore_SelectorBypassesCache(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSelector)
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw).Build()

	store := newMergeViewStore()

	_, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	assert.Empty(t, store.entries, "selector-mode Gateways must not be cached")
}

// TestListenerViewCache_PerPassDedup proves the per-pass cache lists the
// ListenerSets at most once per Gateway, however many times forGateway is
// called within the pass.
func TestListenerViewCache_PerPassDedup(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSame)
	ls := viewListenerSet("ls-a", "a.example.com", 80, gatewayv1.HTTPProtocolType)
	base := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, ls).Build()
	spy := &listSpyClient{Client: base}

	cache := newListenerViewCache(spy, newMergeViewStore())

	for range 5 {
		_, err := cache.forGateway(context.Background(), gw)
		require.NoError(t, err)
	}

	assert.Equal(t, int64(1), spy.listenerSetLists.Load(), "one pass must list ListenerSets once per Gateway")
}

// TestMergeViewStore_ConcurrentResolve exercises the lock under -race.
func TestMergeViewStore_ConcurrentResolve(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSame)
	ls := viewListenerSet("ls-a", "a.example.com", 80, gatewayv1.HTTPProtocolType)
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, ls).Build()

	store := newMergeViewStore()

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, err := store.resolve(context.Background(), cli, gw)
			assert.NoError(t, err)
		})
	}

	wg.Wait()
}

// TestMergeViewStore_RecomputesOnListenerSetRecreate proves the fingerprint
// covers creationTimestamp, not just generation: recreating a ListenerSet under
// the same namespace/name (generation resets to 1) with a later
// creationTimestamp must reorder conflict precedence and force a rebuild — not
// serve the pre-delete merged view.
func TestMergeViewStore_RecomputesOnListenerSetRecreate(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromAll)

	// Two conflicting entries on the same (port, hostname): the earlier-created
	// ListenerSet wins, the later one is annotated HostnameConflict.
	host := gatewayv1.Hostname("clash.example.com")
	lsA := viewListenerSet("ls-a", host, 80, gatewayv1.HTTPProtocolType)
	lsA.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	lsB := viewListenerSet("ls-b", host, 80, gatewayv1.HTTPProtocolType)
	lsB.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))

	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, lsA, lsB).Build()
	store := newMergeViewStore()

	first, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)
	// ls-a (created first) wins; ls-b conflicts.
	assert.Empty(t, first.conflictReason(lsA, "l1"))
	assert.NotEmpty(t, first.conflictReason(lsB, "l1"))

	// Recreate ls-a with a creationTimestamp LATER than ls-b, generation reset
	// to 1 (as the API server would on a fresh create).
	require.NoError(t, cli.Delete(context.Background(), lsA))

	lsARecreated := viewListenerSet("ls-a", host, 80, gatewayv1.HTTPProtocolType)
	lsARecreated.CreationTimestamp = metav1.NewTime(time.Unix(300, 0))
	require.NoError(t, cli.Create(context.Background(), lsARecreated))

	second, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)

	assert.NotSame(t, first, second, "a same-name recreate with a new creationTimestamp must rebuild the view")
	// Precedence flipped: ls-b (created first now) wins; the recreated ls-a conflicts.
	assert.Empty(t, second.conflictReason(lsB, "l1"))
	assert.NotEmpty(t, second.conflictReason(lsARecreated, "l1"))
}

// TestMergeViewStore_Forget proves a cached entry is dropped on forget so the
// store does not retain views for deleted Gateways.
func TestMergeViewStore_Forget(t *testing.T) {
	t.Parallel()

	gw := viewGateway(gatewayv1.NamespacesFromSame)
	ls := viewListenerSet("ls-a", "a.example.com", 80, gatewayv1.HTTPProtocolType)
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).WithObjects(gw, ls).Build()

	store := newMergeViewStore()

	_, err := store.resolve(context.Background(), cli, gw)
	require.NoError(t, err)
	require.NotEmpty(t, store.entries)

	store.forget(client.ObjectKeyFromObject(gw))
	assert.Empty(t, store.entries, "forget must drop the cached entry")

	// forget on a nil store and an unknown key must not panic.
	var nilStore *mergeViewStore

	nilStore.forget(client.ObjectKeyFromObject(gw))
	store.forget(types.NamespacedName{Name: "never-cached", Namespace: "x"})
}

// TestGatewayReconciler_ForgetsViewOnDelete proves the reconciler evicts a
// Gateway's cached merge view when the Gateway is gone (Get → NotFound).
func TestGatewayReconciler_ForgetsViewOnDelete(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Name: "gone", Namespace: "infra"}

	store := newMergeViewStore()
	store.entries[key] = storeEntry{fingerprint: "stale", view: &gatewayListenerView{}}

	// Empty client: the Gateway does not exist, so Reconcile takes the
	// NotFound path.
	cli := fake.NewClientBuilder().WithScheme(viewTestScheme(t)).Build()
	r := &GatewayReconciler{Client: cli, ControllerName: "test-controller", ViewStore: store}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	assert.Empty(t, store.entries, "a deleted Gateway must be evicted from the merge-view store")
}
