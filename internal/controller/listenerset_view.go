package controller

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// gatewayListenerView is the precedence-ordered, conflict-annotated merged
// view of a Gateway plus the ListenerSets allowed to attach to it. It bundles
// the accepted ListenerSets and the single listenermerge.Merge output so the
// status, count, and route-binding consumers can share one computation instead
// of each rebuilding it (issue #332).
//
// A view is immutable after construction. The cross-reconcile mergeViewStore
// retains views across goroutines; List returns deep copies from the informer
// cache, so the *gatewayv1.ListenerSet pointers it holds never alias mutating
// state.
type gatewayListenerView struct {
	acceptedSets []*gatewayv1.ListenerSet
	merged       *listenermerge.MergeResult
}

// conflictReason returns the conflict reason annotated on the merged-view entry
// for (listenerSet, name), or "" when the entry is conflict-free or absent.
func (v *gatewayListenerView) conflictReason(
	listenerSet *gatewayv1.ListenerSet,
	name gatewayv1.SectionName,
) gatewayv1.ListenerConditionReason {
	if v == nil {
		return ""
	}

	entry := findMergedEntry(v.merged, listenerSet, name)
	if entry == nil {
		return ""
	}

	return entry.ConflictReason
}

// buildGatewayListenerView computes the view for a Gateway from scratch: list
// the targeting ListenerSets, keep those the allowedListeners filter accepts,
// and run listenermerge.Merge once. This is the single construction path; the
// caches below only memoize its result.
func buildGatewayListenerView(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) (*gatewayListenerView, error) {
	candidates, err := listTargetingListenerSets(ctx, cli, gateway)
	if err != nil {
		return nil, err
	}

	return viewFromCandidates(ctx, cli, gateway, candidates)
}

// viewFromCandidates builds the view from an already-listed candidate set,
// letting the store reuse the List it performed for fingerprinting.
func viewFromCandidates(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
	candidates []*gatewayv1.ListenerSet,
) (*gatewayListenerView, error) {
	accepted, err := acceptedFromCandidates(ctx, routebinding.NewValidator(cli), gateway, candidates)
	if err != nil {
		return nil, err
	}

	return &gatewayListenerView{
		acceptedSets: accepted,
		merged:       listenermerge.Merge(gateway, accepted),
	}, nil
}

// listenerViewCache memoizes gatewayListenerView for the duration of a single
// reconcile/sync pass. Within one pass a Gateway is resolved (and its
// ListenerSets listed) at most once, even when many routes reference it. On a
// miss it consults the shared store (when present) for cross-reconcile reuse,
// otherwise it builds the view directly.
//
// A nil *listenerViewCache is valid and computes views directly without
// memoizing — call sites that have no pass to scope (e.g. tests) may pass nil.
type listenerViewCache struct {
	cli   client.Client
	store *mergeViewStore
	views map[client.ObjectKey]*gatewayListenerView
}

// newListenerViewCache creates a per-pass cache. store may be nil (no
// cross-reconcile reuse).
func newListenerViewCache(cli client.Client, store *mergeViewStore) *listenerViewCache {
	return &listenerViewCache{
		cli:   cli,
		store: store,
		views: make(map[client.ObjectKey]*gatewayListenerView),
	}
}

// orNew returns the cache when non-nil, otherwise a fresh standalone cache
// bound to cli (no store, no cross-call memoization). Call sites that thread a
// pass-scoped cache pass it through; those without one (e.g. tests) pass nil
// and still get a working, behaviour-identical view.
func (c *listenerViewCache) orNew(cli client.Client) *listenerViewCache {
	if c != nil {
		return c
	}

	return newListenerViewCache(cli, nil)
}

// forGateway returns the merged view for gateway, building it once per pass.
func (c *listenerViewCache) forGateway(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*gatewayListenerView, error) {
	key := client.ObjectKeyFromObject(gateway)
	if view, ok := c.views[key]; ok {
		return view, nil
	}

	view, err := c.resolve(ctx, gateway)
	if err != nil {
		return nil, err
	}

	c.views[key] = view

	return view, nil
}

func (c *listenerViewCache) resolve(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*gatewayListenerView, error) {
	if c.store == nil {
		return buildGatewayListenerView(ctx, c.cli, gateway)
	}

	return c.store.resolve(ctx, c.cli, gateway)
}

// storeEntry pairs a cached view with the fingerprint of the inputs it was
// built from.
type storeEntry struct {
	fingerprint string
	view        *gatewayListenerView
}

// mergeViewStore caches gatewayListenerView across reconciles, keyed by
// Gateway. It is shared by every reconciler/syncer that needs the merged view,
// so a burst of reconciles triggered by one event reuses a single Merge.
//
// Reuse is guarded by a content fingerprint over the Gateway generation and the
// targeting ListenerSets' (namespace, name, generation) — NOT the Gateway
// generation alone, because a sibling ListenerSet spec edit changes the view
// without bumping the Gateway's generation. Any input change flips the
// fingerprint and forces a rebuild, so the store never serves a stale view.
//
// Gateways using allowedListeners From=Selector bypass the store entirely:
// acceptance then depends on Namespace labels, which are not part of the
// fingerprint (and not watched), so caching could outlive a label change.
type mergeViewStore struct {
	mu      sync.Mutex
	entries map[client.ObjectKey]storeEntry
}

// newMergeViewStore creates an empty shared store.
func newMergeViewStore() *mergeViewStore {
	return &mergeViewStore{
		entries: make(map[client.ObjectKey]storeEntry),
	}
}

// forget drops the cached entry for a Gateway. The GatewayReconciler calls it
// when a Gateway is deleted (observed as a NotFound) so the store does not
// retain merged views — and the deep-copied ListenerSets they reference — for
// Gateways that no longer exist. Safe to call on a nil store or for a Gateway
// that was never cached.
func (s *mergeViewStore) forget(key client.ObjectKey) {
	if s == nil {
		return
	}

	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// resolve returns the merged view for gateway, reusing a cached one when the
// inputs are unchanged. It always lists the targeting ListenerSets (a cheap
// informer-cache read) to derive the fingerprint; on a hit it skips the
// acceptance evaluation and the Merge.
func (s *mergeViewStore) resolve(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) (*gatewayListenerView, error) {
	candidates, err := listTargetingListenerSets(ctx, cli, gateway)
	if err != nil {
		return nil, err
	}

	// Selector mode depends on Namespace labels outside the fingerprint —
	// build fresh and do not cache.
	if gatewayUsesNamespaceSelector(gateway) {
		return viewFromCandidates(ctx, cli, gateway, candidates)
	}

	key := client.ObjectKeyFromObject(gateway)
	fingerprint := mergeViewFingerprint(gateway, candidates)

	s.mu.Lock()
	if entry, ok := s.entries[key]; ok && entry.fingerprint == fingerprint {
		s.mu.Unlock()

		return entry.view, nil
	}
	s.mu.Unlock()

	// Build outside the lock. Concurrent builders for the same Gateway may
	// observe different informer-cache snapshots, so last-writer-wins can store
	// the older view; this self-corrects because the update that produced the
	// newer state also enqueued a reconcile that recomputes the fingerprint.
	view, err := viewFromCandidates(ctx, cli, gateway, candidates)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.entries[key] = storeEntry{fingerprint: fingerprint, view: view}
	s.mu.Unlock()

	return view, nil
}

// gatewayUsesNamespaceSelector reports whether the Gateway's allowedListeners
// filter selects ListenerSet namespaces by label selector.
func gatewayUsesNamespaceSelector(gateway *gatewayv1.Gateway) bool {
	allowed := gateway.Spec.AllowedListeners

	return allowed != nil &&
		allowed.Namespaces != nil &&
		allowed.Namespaces.From != nil &&
		*allowed.Namespaces.From == gatewayv1.NamespacesFromSelector
}

// mergeViewFingerprint produces a canonical key over everything the merged view
// depends on: the Gateway generation (covers spec.listeners and
// spec.allowedListeners) and, per targeting ListenerSet, its identity +
// generation + creationTimestamp.
//
// Generation covers spec.listeners and spec.parentRef of a live object.
// creationTimestamp is in the token because Merge orders ListenerSets by it
// (see listenermerge.sortListenerSets), which decides conflict precedence — and
// it is NOT redundant with generation across a delete+recreate: a ListenerSet
// recreated under the same namespace/name resets its generation to 1 but gets a
// fresh creationTimestamp, so without it the fingerprint would collide and the
// store could serve a stale precedence ordering.
func mergeViewFingerprint(gateway *gatewayv1.Gateway, candidates []*gatewayv1.ListenerSet) string {
	keys := make([]string, 0, len(candidates))
	for _, ls := range candidates {
		keys = append(keys, ls.Namespace+"/"+ls.Name+
			"@"+strconv.FormatInt(ls.Generation, 10)+
			"#"+strconv.FormatInt(ls.CreationTimestamp.UnixNano(), 10))
	}

	slices.Sort(keys)

	var builder strings.Builder

	builder.WriteString("g@")
	builder.WriteString(strconv.FormatInt(gateway.Generation, 10))
	builder.WriteByte(';')
	builder.WriteString(strings.Join(keys, ";"))

	return builder.String()
}
