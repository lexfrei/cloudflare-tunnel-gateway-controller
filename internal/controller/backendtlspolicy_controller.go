package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// configMapCAKey is the well-known key inside a ConfigMap that holds the PEM
// CA bundle, per Gateway API Core support level for BackendTLSPolicy.
const configMapCAKey = "ca.crt"

// policyAncestorStatusMaxCount caps Status.Ancestors per Gateway API spec
// (MaxItems=16 on PolicyStatus.Ancestors). The API server would otherwise
// reject status updates on policies that front more than 16 Gateways. Per
// spec we MUST NOT add further entries when full; this implementation
// preserves every other controller's entry fully and only truncates OUR
// entries (sorted by {namespace, name} for determinism) to fit within the
// remaining slots. Operators of co-installed Gateway implementations never
// have their claims clobbered by ours.
const policyAncestorStatusMaxCount = 16

// Sentinel errors for BackendTLSPolicy CA validation so wrappers can be matched.
var (
	errBackendTLSNoCARef           = errors.New("BackendTLSPolicy has no CACertificateRefs (WellKnownCACertificates not supported)")
	errBackendTLSUnsupportedGroup  = errors.New("BackendTLSPolicy CACertificateRef group not supported (only core)")
	errBackendTLSUnsupportedKind   = errors.New("BackendTLSPolicy CACertificateRef kind not supported (only ConfigMap)")
	errBackendTLSCAKeyMissing      = errors.New("BackendTLSPolicy CA ConfigMap is missing the ca.crt key")
	errBackendTLSCABundleMalformed = errors.New("BackendTLSPolicy CA bundle is not valid PEM")
	errBackendTLSCABundleNoCerts   = errors.New("BackendTLSPolicy CA bundle contains no CERTIFICATE blocks")
)

// parseCABundle decodes every PEM block in the supplied bundle and verifies
// that at least one of them is a parseable CERTIFICATE. It is intentionally
// strict: a bundle containing exclusively non-CERTIFICATE blocks (or no PEM
// blocks at all) is rejected so the operator sees Accepted=False rather than
// silently shipping an empty trust pool to the proxy.
func parseCABundle(bundle string) (int, error) {
	rest := []byte(bundle)
	parsed := 0

	for {
		var block *pem.Block

		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}

		if block.Type != "CERTIFICATE" {
			continue
		}

		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return parsed, fmt.Errorf("%w (block %d): %w", errBackendTLSCABundleMalformed, parsed, err)
		}

		parsed++
	}

	if parsed == 0 {
		return 0, errBackendTLSCABundleNoCerts
	}

	return parsed, nil
}

// getConfigMap fetches a ConfigMap by namespaced key. Returns the wrapped
// apierror unchanged so callers can switch on apierrors.IsNotFound.
func getConfigMap(ctx context.Context, c client.Client, key client.ObjectKey) (*corev1.ConfigMap, error) {
	var configMap corev1.ConfigMap
	if err := c.Get(ctx, key, &configMap); err != nil {
		return nil, fmt.Errorf("get configmap %s/%s: %w", key.Namespace, key.Name, err)
	}

	return &configMap, nil
}

// routeReferencesAnyService reports whether the HTTPRoute references any of
// the named Services (in the targetNamespace) as a backend. Service-only refs
// are considered (group "" or "core", kind "" or "Service"). The backendRef's
// namespace is checked too: a route with backendRef
// {Name: "svc", Namespace: "other-ns"} MUST NOT match a policy targeting "svc"
// in targetNamespace, because the policy applies to its own-namespace Service
// only (LocalPolicyTargetReferenceWithSectionName carries no Namespace field
// per the BackendTLSPolicy spec).
func routeReferencesAnyService(route *gatewayv1.HTTPRoute, targets map[string]struct{}, targetNamespace string) bool {
	if len(targets) == 0 {
		return false
	}

	for ruleIdx := range route.Spec.Rules {
		for refIdx := range route.Spec.Rules[ruleIdx].BackendRefs {
			ref := &route.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef
			if backendRefMatchesTargetSet(ref.BackendObjectReference, route.Namespace, targets, targetNamespace) {
				return true
			}
		}
	}

	return false
}

// grpcRouteReferencesAnyService is the GRPCRoute counterpart of
// routeReferencesAnyService — same namespace-aware contract.
func grpcRouteReferencesAnyService(route *gatewayv1.GRPCRoute, targets map[string]struct{}, targetNamespace string) bool {
	if len(targets) == 0 {
		return false
	}

	for ruleIdx := range route.Spec.Rules {
		for refIdx := range route.Spec.Rules[ruleIdx].BackendRefs {
			ref := &route.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef
			if backendRefMatchesTargetSet(ref.BackendObjectReference, route.Namespace, targets, targetNamespace) {
				return true
			}
		}
	}

	return false
}

// backendRefMatchesTargetSet reports whether the supplied backend object
// reference points at one of the Services in targets AND lives in
// targetNamespace. The route's own namespace is used as the default
// when the ref omits Namespace (per spec). Non-Service kinds are
// skipped via IsServiceBackendRef.
func backendRefMatchesTargetSet(ref gatewayv1.BackendObjectReference, routeNamespace string, targets map[string]struct{}, targetNamespace string) bool {
	if !proxy.IsServiceBackendRef(ref) {
		return false
	}

	refNamespace := routeNamespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	if refNamespace != targetNamespace {
		return false
	}

	_, hit := targets[string(ref.Name)]

	return hit
}

// parentReferenceToKey resolves an HTTPRoute parentRef into a ClusterObjectKey,
// using the route's own namespace when the ref omits the namespace field.
func parentReferenceToKey(parentRef gatewayv1.ParentReference, routeNamespace string) client.ObjectKey {
	namespace := routeNamespace
	if parentRef.Namespace != nil {
		namespace = string(*parentRef.Namespace)
	}

	return client.ObjectKey{Namespace: namespace, Name: string(parentRef.Name)}
}

// parentRefIsGateway reports whether the parentRef targets a Gateway
// (Group "" / gateway.networking.k8s.io, Kind "" / "Gateway"). Filters
// non-Gateway parents (ListenerSet, future kinds) out of the BackendTLS
// Policy Ancestor walk so the subsequent Gateway Get does not waste a
// round-trip on a guaranteed 404 — which would have silently dropped
// the entry, masking the leak but leaving noisy reconciles.
func parentRefIsGateway(parentRef gatewayv1.ParentReference) bool {
	if parentRef.Group != nil && *parentRef.Group != "" && *parentRef.Group != gatewayv1.GroupName {
		return false
	}

	if parentRef.Kind != nil && *parentRef.Kind != "" && *parentRef.Kind != kindGateway {
		return false
	}

	return true
}

// collectGatewayParentKeys folds every Gateway-shaped parentRef into the
// supplied key set, using the route's own namespace when the ref omits
// the namespace field. Non-Gateway parents are skipped explicitly per
// parentRefIsGateway.
func collectGatewayParentKeys(set map[client.ObjectKey]struct{}, parentRefs []gatewayv1.ParentReference, routeNamespace string) {
	for _, parentRef := range parentRefs {
		if !parentRefIsGateway(parentRef) {
			continue
		}

		set[parentReferenceToKey(parentRef, routeNamespace)] = struct{}{}
	}
}

// BackendTLSPolicyReconciler maintains the status of BackendTLSPolicy
// resources: validates the CA references, computes Accepted and ResolvedRefs
// conditions, and writes them under each affected Gateway as a policy ancestor.
type BackendTLSPolicyReconciler struct {
	client.Client

	Scheme         *runtime.Scheme
	ControllerName string
}

// Reconcile validates a BackendTLSPolicy against the cluster's current state
// and refreshes its status conditions for every Gateway that fronts a route to
// the policy's target Service. Resources we do not manage are ignored.
func (r *BackendTLSPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy gatewayv1.BackendTLSPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get BackendTLSPolicy")
	}

	gateways, err := r.gatewaysForPolicy(ctx, &policy)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to enumerate ancestor gateways")
	}

	if len(gateways) == 0 {
		// No Gateway from our class references the target — nothing to claim.
		return ctrl.Result{}, nil
	}

	conditions := r.computeConditions(ctx, &policy)
	logger.Info("reconciling BackendTLSPolicy",
		"name", policy.Name,
		"namespace", policy.Namespace,
		"gateways", len(gateways),
	)

	if err := r.updateStatus(ctx, req.NamespacedName, gateways, conditions, policy.Generation); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update BackendTLSPolicy status")
	}

	return ctrl.Result{}, nil
}

// computeConditions evaluates the policy spec and returns the two conditions
// to expose (Accepted and ResolvedRefs) per Gateway API semantics.
//
//   - CA reference invalid or unresolvable: Accepted=False, Reason=NoValidCACertificate;
//     ResolvedRefs=False, Reason=InvalidCACertificateRef (or InvalidKind for
//     Group/Kind mismatches).
//   - Conflict with an older peer policy on at least one shared (Service,
//     SectionName) target: Accepted=False, Reason=Conflicted; ResolvedRefs
//     stays True because the policy's own refs are valid.
//   - All happy: both True. Both DNS-Hostname and URI-type SubjectAltNames
//     are honoured end-to-end by the proxy.
//
// CA validity is checked first — Reason=InvalidCACertificateRef (or
// InvalidKind / NoValidCACertificate) dominates over Conflicted, because a
// policy with a broken CA cannot be Accepted=True regardless of whether
// another peer also targets the same Service. Operators see the actionable
// error first; Conflicted is only emitted on policies that would otherwise
// be Accepted=True.
//
// LastTransitionTime is left zero; callers route through meta.SetStatusCondition
// in updateStatus so the timestamp reflects an actual transition rather than
// flapping on every reconcile.
func (r *BackendTLSPolicyReconciler) computeConditions(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) []metav1.Condition {
	// WellKnownCACertificates is not supported — only explicit CACertificateRefs
	// are honoured. The CRD CEL admits a WellKnown-only policy (empty
	// caCertificateRefs + wellKnownCACertificates set), and the Gateway API spec
	// mandates Accepted=False/Invalid for an unsupported WellKnown value, not the
	// generic NoValidCACertificate that an empty-refs policy would otherwise get.
	if len(policy.Spec.Validation.CACertificateRefs) == 0 && policy.Spec.Validation.WellKnownCACertificates != nil {
		return wellKnownUnsupportedConditions(policy.Generation, *policy.Spec.Validation.WellKnownCACertificates)
	}

	if err := r.validateCARefs(ctx, policy); err != nil {
		return caInvalidConditions(policy.Generation, err)
	}

	if winner := r.conflictWinnerFor(ctx, policy); winner != nil {
		return conflictedConditions(policy.Generation, winner)
	}

	return acceptedConditions(policy.Generation)
}

// conflictedConditions returns the Accepted=False/Reason=Conflicted +
// ResolvedRefs=True pair stamped on a BackendTLSPolicy that lost the
// precedence comparison against a peer targeting the same (Service,
// SectionName). ResolvedRefs stays True because the loser's own CA refs
// resolved cleanly — the conflict is about precedence, not about the
// loser's own validity.
func conflictedConditions(generation int64, winner *gatewayv1.BackendTLSPolicy) []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               string(gatewayv1.PolicyConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.PolicyReasonConflicted),
			Message: fmt.Sprintf("conflicts with BackendTLSPolicy %s/%s",
				winner.Namespace, winner.Name),
		},
		{
			Type:               string(gatewayv1.BackendTLSPolicyConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.BackendTLSPolicyReasonResolvedRefs),
			Message:            backendTLSResolvedRefsMessage,
		},
	}
}

// conflictWinnerFor returns the peer BackendTLSPolicy that wins the
// precedence comparison against `policy` on at least one shared
// (Service, SectionName) target, or nil if `policy` itself wins or no
// peers conflict.
//
// Fails open: a cluster-list error is logged and treated as "no
// conflict" so Status does not flip on a transient cache miss. The
// caller continues to Accepted=True, the next reconcile re-runs the
// check, and the proxy-side resolver already enforces precedence
// independently — there is no plaintext-bypass risk from a missed
// Conflicted stamp.
func (r *BackendTLSPolicyReconciler) conflictWinnerFor(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) *gatewayv1.BackendTLSPolicy {
	ownTargets := normalizePolicyTargets(policy)
	if len(ownTargets) == 0 {
		return nil
	}

	var list gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &list, client.InNamespace(policy.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list BackendTLSPolicies for conflict check failed; treating as no conflict",
			"namespace", policy.Namespace, "policy", policy.Name)

		return nil
	}

	var winner *gatewayv1.BackendTLSPolicy

	for peerIdx := range list.Items {
		peer := &list.Items[peerIdx]
		if peer.Name == policy.Name && peer.Namespace == policy.Namespace {
			continue
		}

		if !policiesShareTarget(peer, ownTargets) {
			continue
		}

		// We only care about peers strictly older than `policy` — peers
		// younger than us are themselves the losers, and they will see us
		// as the winner on their own reconcile.
		if !isPolicyOlder(peer, policy) {
			continue
		}

		if winner == nil || isPolicyOlder(peer, winner) {
			winner = peer
		}
	}

	return winner
}

// normalizePolicyTargets canonicalises a policy's Service-shaped
// TargetRefs to a deduplicated set of (Name, SectionName) keys.
// Non-Service kinds are skipped (BackendTLSPolicy's Standard channel
// only supports Service targets today). SectionName comparison is
// literal — a policy without SectionName covers ALL ports of the
// Service, but per GEP-713 it does NOT collide with a separate policy
// that scopes itself to a specific named port (different scopes ⇒ no
// conflict).
//
// Mismatch with the runtime resolver, by design: selectPolicyForServicePort
// (internal/controller/proxy_syncer.go) resolves SectionName against the
// actual Service port-name via a Service Get and matches when
// SectionName == port-name. So a scoped (SectionName="https") and an
// unscoped policy on a Service with a port named "https" both reach the
// resolver for that port at runtime, where the older one wins. This
// status-side mapper deliberately treats those as different scopes per
// the spec, even though the proxy-side resolver sees the overlap. A
// future maintainer reconciling the two layers (either by consulting
// port names here, or by rephrasing the runtime selection to skip the
// unscoped policy when a scoped one matches) should treat both call
// sites as a pair.
func normalizePolicyTargets(policy *gatewayv1.BackendTLSPolicy) map[targetKey]struct{} {
	keys := map[targetKey]struct{}{}

	for _, target := range policy.Spec.TargetRefs {
		kind := string(target.Kind)
		if kind != "" && kind != serviceKind {
			continue
		}

		key := targetKey{name: string(target.Name)}
		if target.SectionName != nil {
			key.section = string(*target.SectionName)
		}

		keys[key] = struct{}{}
	}

	return keys
}

// policiesShareTarget reports whether `peer` has at least one
// Service-shaped TargetRef that literally matches any (Name,
// SectionName) key in `ownTargets`.
func policiesShareTarget(peer *gatewayv1.BackendTLSPolicy, ownTargets map[targetKey]struct{}) bool {
	for key := range normalizePolicyTargets(peer) {
		if _, ok := ownTargets[key]; ok {
			return true
		}
	}

	return false
}

// targetKey is the canonical conflict-comparison key for a
// BackendTLSPolicy TargetRef: the Service Name and its SectionName
// (empty string when unset, covering all ports of the Service).
type targetKey struct {
	name    string
	section string
}

// caInvalidConditions returns the Accepted=False/NoValidCACertificate +
// ResolvedRefs=False conditions, picking the most specific ResolvedRefs Reason
// from the underlying validation error. Group/Kind mismatches map to
// InvalidKind per the conformance suite; everything else (missing CM, missing
// or empty ca.crt, malformed PEM) falls back to InvalidCACertificateRef.
func caInvalidConditions(generation int64, err error) []metav1.Condition {
	resolvedRefsReason := gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef
	if errors.Is(err, errBackendTLSUnsupportedKind) || errors.Is(err, errBackendTLSUnsupportedGroup) {
		resolvedRefsReason = gatewayv1.BackendTLSPolicyReasonInvalidKind
	}

	return []metav1.Condition{
		{
			Type:               string(gatewayv1.PolicyConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate),
			Message:            err.Error(),
		},
		{
			Type:               string(gatewayv1.BackendTLSPolicyConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             string(resolvedRefsReason),
			Message:            err.Error(),
		},
	}
}

// wellKnownUnsupportedConditions returns the Accepted=False/Invalid +
// ResolvedRefs=False pair for a policy that relies solely on
// WellKnownCACertificates, which this controller does not support. Per the
// Gateway API spec (backendtlspolicy_types.go:206-209) an implementation that
// does not support WellKnownCACertificates MUST set Accepted=False with
// Reason=Invalid; the generic NoValidCACertificate reason would mislead the
// operator into hunting for a CA ref that the policy never declared.
func wellKnownUnsupportedConditions(generation int64, value gatewayv1.WellKnownCACertificatesType) []metav1.Condition {
	msg := fmt.Sprintf(
		"WellKnownCACertificates %q is not supported; configure explicit caCertificateRefs instead", value)

	return []metav1.Condition{
		{
			Type:               string(gatewayv1.PolicyConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.PolicyReasonInvalid),
			Message:            msg,
		},
		{
			Type:               string(gatewayv1.BackendTLSPolicyConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate),
			Message:            msg,
		},
	}
}

// backendTLSResolvedRefsMessage is the BackendTLSPolicy-specific Message
// for the ResolvedRefs=True condition. It is more specific than the
// generic resolvedRefsMessage shared with HTTPRoute / Gateway, because
// for this policy "references" unambiguously means CA certificate refs.
// Both happy-path and Conflicted-loser conditions reuse the same string
// — the loser's own CA refs do resolve, only the precedence comparison
// rejects it.
const backendTLSResolvedRefsMessage = "All CA certificate references resolved"

// acceptedConditions returns the happy-path Accepted=True + ResolvedRefs=True pair.
func acceptedConditions(generation int64) []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               string(gatewayv1.PolicyConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.PolicyReasonAccepted),
			Message:            "BackendTLSPolicy CA references resolved",
		},
		{
			Type:               string(gatewayv1.BackendTLSPolicyConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             string(gatewayv1.BackendTLSPolicyReasonResolvedRefs),
			Message:            backendTLSResolvedRefsMessage,
		},
	}
}

// validateCARefs returns an error describing the first invalid CA reference,
// or nil when every reference resolves to a ConfigMap whose "ca.crt" key
// contains at least one parseable PEM CERTIFICATE block. Only same-namespace
// ConfigMap refs are supported (Core).
func (r *BackendTLSPolicyReconciler) validateCARefs(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) error {
	refs := policy.Spec.Validation.CACertificateRefs
	if len(refs) == 0 {
		return errBackendTLSNoCARef
	}

	for _, ref := range refs {
		group := string(ref.Group)
		if group != "" && group != coreGroup {
			return fmt.Errorf("%w: %q", errBackendTLSUnsupportedGroup, group)
		}

		if string(ref.Kind) != configMapKind {
			return fmt.Errorf("%w: %q", errBackendTLSUnsupportedKind, ref.Kind)
		}

		key := client.ObjectKey{Namespace: policy.Namespace, Name: string(ref.Name)}

		configMap, err := getConfigMap(ctx, r.Client, key)
		if err != nil {
			return err
		}

		bundle := configMap.Data[configMapCAKey]
		if bundle == "" {
			return fmt.Errorf("%w: %s/%s key %q is empty or missing",
				errBackendTLSCAKeyMissing, key.Namespace, key.Name, configMapCAKey)
		}

		if _, err := parseCABundle(bundle); err != nil {
			return fmt.Errorf("ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return nil
}

// gatewaysForPolicy returns the Gateways managed by this controller that
// front any HTTPRoute referencing the policy's target Service. We carry that
// list into Status.Ancestors as the policy's parent context.
func (r *BackendTLSPolicyReconciler) gatewaysForPolicy(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) ([]gatewayv1.Gateway, error) {
	if len(policy.Spec.TargetRefs) == 0 {
		return nil, nil
	}

	targetServices := make(map[string]struct{}, len(policy.Spec.TargetRefs))

	for _, target := range policy.Spec.TargetRefs {
		kind := string(target.Kind)
		if kind != "" && kind != serviceKind {
			continue
		}

		targetServices[string(target.Name)] = struct{}{}
	}

	var httpRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRoutes, client.InNamespace(policy.Namespace)); err != nil {
		return nil, fmt.Errorf("list httproutes: %w", err)
	}

	var grpcRoutes gatewayv1.GRPCRouteList
	if err := r.List(ctx, &grpcRoutes, client.InNamespace(policy.Namespace)); err != nil {
		return nil, fmt.Errorf("list grpcroutes: %w", err)
	}

	gatewayKeys := collectGatewayKeys(httpRoutes.Items, grpcRoutes.Items, targetServices, policy.Namespace)

	gateways := make([]gatewayv1.Gateway, 0, len(gatewayKeys))

	for _, key := range gatewayKeys {
		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, key, &gateway); err != nil {
			continue
		}

		managed, err := r.gatewayManagedByUs(ctx, &gateway)
		if err != nil || !managed {
			continue
		}

		gateways = append(gateways, gateway)
	}

	return gateways, nil
}

// collectGatewayKeys returns the deterministic, sorted set of Gateway keys
// reached by HTTPRoutes and GRPCRoutes that reference any of the target
// services. Sorting by {namespace, name} keeps Status.Ancestors stable
// across reconciles, which matters once a policy fronts more than 16
// Gateways and updateStatus has to truncate.
func collectGatewayKeys(
	httpRoutes []gatewayv1.HTTPRoute,
	grpcRoutes []gatewayv1.GRPCRoute,
	targetServices map[string]struct{},
	targetNamespace string,
) []client.ObjectKey {
	gatewayKeySet := map[client.ObjectKey]struct{}{}

	for routeIdx := range httpRoutes {
		route := &httpRoutes[routeIdx]
		if !routeReferencesAnyService(route, targetServices, targetNamespace) {
			continue
		}

		collectGatewayParentKeys(gatewayKeySet, route.Spec.ParentRefs, route.Namespace)
	}

	for routeIdx := range grpcRoutes {
		route := &grpcRoutes[routeIdx]
		if !grpcRouteReferencesAnyService(route, targetServices, targetNamespace) {
			continue
		}

		collectGatewayParentKeys(gatewayKeySet, route.Spec.ParentRefs, route.Namespace)
	}

	gatewayKeys := make([]client.ObjectKey, 0, len(gatewayKeySet))
	for key := range gatewayKeySet {
		gatewayKeys = append(gatewayKeys, key)
	}

	sort.Slice(gatewayKeys, func(left, right int) bool {
		if gatewayKeys[left].Namespace != gatewayKeys[right].Namespace {
			return gatewayKeys[left].Namespace < gatewayKeys[right].Namespace
		}

		return gatewayKeys[left].Name < gatewayKeys[right].Name
	})

	return gatewayKeys
}

// gatewayManagedByUs reports whether the Gateway's GatewayClass binds to this
// controller. Matches the existing pattern used by mappers and reconcilers.
func (r *BackendTLSPolicyReconciler) gatewayManagedByUs(ctx context.Context, gateway *gatewayv1.Gateway) (bool, error) {
	var gatewayClass gatewayv1.GatewayClass

	key := client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}
	if err := r.Get(ctx, key, &gatewayClass); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get GatewayClass %s: %w", key.Name, err)
	}

	return string(gatewayClass.Spec.ControllerName) == r.ControllerName, nil
}

// updateStatus replaces this controller's entries in Status.Ancestors with one
// entry per managed Gateway, carrying the supplied conditions. Other
// controllers' entries are preserved. meta.SetStatusCondition is used to
// merge each condition into the existing ancestor (when present), preserving
// LastTransitionTime when neither Status, Reason, nor Message changed.
func (r *BackendTLSPolicyReconciler) updateStatus(
	ctx context.Context,
	policyKey client.ObjectKey,
	gateways []gatewayv1.Gateway,
	conditions []metav1.Condition,
	reconciledGen int64,
) error {
	//nolint:wrapcheck // retry wrapper handles errors internally
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gatewayv1.BackendTLSPolicy
		if err := r.Get(ctx, policyKey, &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			return err //nolint:wrapcheck // unwrapped to participate in retry
		}

		// Split current ancestors into our previous conditions (for merge) and
		// other controllers' entries (preserved). stale=true means a newer
		// reconcile already advanced our status, so we MUST NOT overwrite it.
		existing, otherControllerEntries, stale := r.partitionAncestors(&fresh, reconciledGen)
		if stale {
			return nil
		}

		ourEntries := make([]gatewayv1.PolicyAncestorStatus, 0, len(gateways))

		for gwIdx := range gateways {
			gateway := &gateways[gwIdx]
			ancestorRef := gatewayAncestorRef(gateway)

			key := client.ObjectKey{Namespace: gateway.Namespace, Name: gateway.Name}
			merged := append([]metav1.Condition(nil), existing[key]...)

			for _, condition := range conditions {
				meta.SetStatusCondition(&merged, condition)
			}

			ourEntries = append(ourEntries, gatewayv1.PolicyAncestorStatus{
				AncestorRef:    ancestorRef,
				ControllerName: gatewayv1.GatewayController(r.ControllerName),
				Conditions:     merged,
			})
		}

		// Reserve the full slot count for other controllers' entries first;
		// only OUR entries get truncated when the combined set exceeds the
		// spec's 16-entry cap. Other controllers' status MUST NOT be dropped
		// by our reconciler.
		available := max(policyAncestorStatusMaxCount-len(otherControllerEntries), 0)

		if len(ourEntries) > available {
			ourEntries = ourEntries[:available]
		}

		combined := otherControllerEntries
		combined = append(combined, ourEntries...)
		fresh.Status.Ancestors = combined

		return r.Status().Update(ctx, &fresh) //nolint:wrapcheck // unwrapped to participate in retry
	})
}

// partitionAncestors splits the policy's current ancestors into this
// controller's previous conditions (keyed by Gateway, so SetStatusCondition can
// preserve LastTransitionTime on merge) and the entries owned by other
// controllers (preserved verbatim). stale is true when any of our entries was
// already stamped with a generation newer than reconciledGen, in which case the
// caller MUST NOT overwrite the status (observedGeneration regression guard).
func (r *BackendTLSPolicyReconciler) partitionAncestors(
	fresh *gatewayv1.BackendTLSPolicy,
	reconciledGen int64,
) (map[client.ObjectKey][]metav1.Condition, []gatewayv1.PolicyAncestorStatus, bool) {
	existing := map[client.ObjectKey][]metav1.Condition{}
	others := make([]gatewayv1.PolicyAncestorStatus, 0, len(fresh.Status.Ancestors))

	for _, ancestor := range fresh.Status.Ancestors {
		if string(ancestor.ControllerName) != r.ControllerName {
			others = append(others, ancestor)

			continue
		}

		if statusGenerationStale(reconciledGen, ancestor.Conditions) {
			return existing, others, true
		}

		key := ancestorRefKey(ancestor.AncestorRef, fresh.Namespace)
		existing[key] = ancestor.Conditions
	}

	return existing, others, false
}

// gatewayAncestorRef returns the ParentReference identifying the supplied
// Gateway as a BackendTLSPolicy ancestor.
func gatewayAncestorRef(gateway *gatewayv1.Gateway) gatewayv1.ParentReference {
	gatewayGroup := gatewayv1.GroupName
	gatewayKind := gatewayv1.Kind("Gateway")
	gatewayNamespace := gatewayv1.Namespace(gateway.Namespace)

	return gatewayv1.ParentReference{
		Group:     (*gatewayv1.Group)(&gatewayGroup),
		Kind:      &gatewayKind,
		Namespace: &gatewayNamespace,
		Name:      gatewayv1.ObjectName(gateway.Name),
	}
}

// ancestorRefKey resolves an AncestorRef back to a {Namespace, Name} key.
// Falls back to the policy's own namespace when the ref omits Namespace.
func ancestorRefKey(ref gatewayv1.ParentReference, policyNamespace string) client.ObjectKey {
	namespace := policyNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	return client.ObjectKey{Namespace: namespace, Name: string(ref.Name)}
}

// setupStatusReconcilers builds and registers the reconcilers responsible only
// for updating status conditions on Gateway API resources we don't otherwise
// drive (GatewayClass acceptance, BackendTLSPolicy ancestry). Extracted from
// the top-level Run() to keep its cyclomatic complexity within budget.
func setupStatusReconcilers(mgr ctrl.Manager, controllerName string) error {
	gatewayClassReconciler := &GatewayClassReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: controllerName,
		// APIReader is uncached: the SupportedVersion check reads a single CRD
		// on demand, so there is no reason to watch every CRD cluster-wide.
		BundleVersionReader: mgr.GetAPIReader(),
	}

	if err := gatewayClassReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gatewayclass controller")
	}

	backendTLSPolicyReconciler := &BackendTLSPolicyReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: controllerName,
	}

	if err := backendTLSPolicyReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup BackendTLSPolicy controller")
	}

	return nil
}

// SetupWithManager wires the reconciler with watches for BackendTLSPolicy,
// HTTPRoutes (target-service membership), and ConfigMaps (CA bundle source).
// The ConfigMap watch is what lets policy status flip from
// NoValidCACertificate to Accepted when an absent CA ConfigMap is later
// created (or back, when its ca.crt key is emptied).
//
// The second Watches on BackendTLSPolicy itself (with the peer-change
// mapper) is what lets a loser flip back to Accepted=True when its older
// sibling — the conflict winner — is deleted. The implicit watch from
// For() only enqueues the policy whose own object changed; without the
// peer mapper, deleting the winner would leave the loser stuck on
// Reason=Conflicted forever.
func (r *BackendTLSPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.BackendTLSPolicy{}).
		Watches(
			&gatewayv1.BackendTLSPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForPeerChange),
		).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForRouteChange),
		).
		Watches(
			&gatewayv1.GRPCRoute{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForGRPCRouteChange),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForConfigMapChange),
		).
		Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed to setup BackendTLSPolicy controller")
	}

	return nil
}

// policiesForPeerChange enqueues every BackendTLSPolicy in the changed
// policy's namespace that shares at least one (Service, SectionName)
// target with it — excluding the changed policy itself, which the
// implicit For() watch already enqueues. Fires on create / update /
// delete; the create + delete paths are what guarantees the loser flips
// status when its winner appears or disappears. Update events are also
// covered so a peer's creationTimestamp change (rare, but possible via
// an admin re-create) re-evaluates precedence.
//
// Cost: O(N) per call (one List + one walk of N peers, each peer's
// normalizePolicyTargets is O(targetRefs)), and the resulting enqueues
// each run a full Reconcile that internally is O(N). Worst-case O(N^2)
// reconciles per policy mutation in a namespace where every policy
// targets overlapping Services. Acceptable for realistic N (a single
// Gateway / Cloudflare account rarely fronts hundreds of policies in
// one namespace); call out here so a future maintainer hitting this in
// a profile knows the trade-off was deliberate.
func (r *BackendTLSPolicyReconciler) policiesForPeerChange(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*gatewayv1.BackendTLSPolicy)
	if !ok {
		return nil
	}

	ownTargets := normalizePolicyTargets(changed)
	if len(ownTargets) == 0 {
		return nil
	}

	var policies gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(changed.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list BackendTLSPolicies for peer-change failed",
			"namespace", changed.Namespace, "policy", changed.Name)

		return nil
	}

	requests := make([]reconcile.Request, 0)

	for policyIdx := range policies.Items {
		peer := &policies.Items[policyIdx]
		if peer.Name == changed.Name && peer.Namespace == changed.Namespace {
			continue
		}

		if !policiesShareTarget(peer, ownTargets) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: peer.Namespace, Name: peer.Name},
		})
	}

	return requests
}

// policiesForConfigMapChange enqueues every BackendTLSPolicy in the changed
// ConfigMap's namespace that references the ConfigMap by name as a CA source.
// Per Gateway API Core, only same-namespace ConfigMap refs are supported, so
// the namespace check matches the reconciler's own validateCARefs scope.
func (r *BackendTLSPolicyReconciler) policiesForConfigMapChange(ctx context.Context, obj client.Object) []reconcile.Request {
	configMap, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}

	var policies gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(configMap.Namespace)); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for policyIdx := range policies.Items {
		policy := &policies.Items[policyIdx]
		if !policyReferencesConfigMap(policy, configMap.Name) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: policy.Namespace, Name: policy.Name},
		})
	}

	return requests
}

// isConfigMapReferencedByBackendTLSPolicy reports whether the supplied
// ConfigMap is referenced as a CA bundle source by any BackendTLSPolicy in
// the same namespace. Used by the route reconciler's ConfigMap watch
// predicate to suppress kube-root-ca, leader-election, and other unrelated
// ConfigMap events that would otherwise trigger a full proxy resync.
func isConfigMapReferencedByBackendTLSPolicy(ctx context.Context, c client.Client, configMap *corev1.ConfigMap) bool {
	var policies gatewayv1.BackendTLSPolicyList
	if err := c.List(ctx, &policies, client.InNamespace(configMap.Namespace)); err != nil {
		return false
	}

	for policyIdx := range policies.Items {
		if policyReferencesConfigMap(&policies.Items[policyIdx], configMap.Name) {
			return true
		}
	}

	return false
}

// policyReferencesConfigMap reports whether the policy lists the named
// ConfigMap among its CACertificateRefs (group ""/"core", kind "ConfigMap").
func policyReferencesConfigMap(policy *gatewayv1.BackendTLSPolicy, configMapName string) bool {
	for _, ref := range policy.Spec.Validation.CACertificateRefs {
		group := string(ref.Group)
		if group != "" && group != coreGroup {
			continue
		}

		if string(ref.Kind) != configMapKind {
			continue
		}

		if string(ref.Name) == configMapName {
			return true
		}
	}

	return false
}

// policiesForRouteChange enqueues only the BackendTLSPolicies in the route's
// namespace whose TargetRefs intersect the route's backendRefs. A change to a
// route that doesn't touch a policy's target Service should not bump that
// policy's status (saves the reconciler from doing full work on every
// unrelated route edit in busy namespaces).
func (r *BackendTLSPolicyReconciler) policiesForRouteChange(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	var policies gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(route.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list BackendTLSPolicies for HTTPRoute change failed; enqueue skipped, next reconcile from another source recovers",
			"namespace", route.Namespace, "route", route.Name)

		return nil
	}

	requests := make([]reconcile.Request, 0, len(policies.Items))

	for policyIdx := range policies.Items {
		policy := &policies.Items[policyIdx]
		if !policyTargetsAnyRouteBackend(policy, route) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: policy.Namespace, Name: policy.Name},
		})
	}

	return requests
}

// policyTargetsAnyRouteBackend reports whether the policy's TargetRefs include
// at least one Service that the route also references as a backend. Used by
// policiesForRouteChange to skip irrelevant policies on every HTTPRoute edit.
func policyTargetsAnyRouteBackend(policy *gatewayv1.BackendTLSPolicy, route *gatewayv1.HTTPRoute) bool {
	return routeReferencesAnyService(route, policyTargetServiceNames(policy), policy.Namespace)
}

// policiesForGRPCRouteChange is the GRPCRoute counterpart of
// policiesForRouteChange — enqueues only the BackendTLSPolicies whose
// TargetRefs intersect the GRPCRoute's backendRefs. Without this watch
// a GRPCRoute referencing a Service freshly targeted by a policy would
// not re-enqueue the policy and the Ancestor list would remain stale.
func (r *BackendTLSPolicyReconciler) policiesForGRPCRouteChange(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.GRPCRoute)
	if !ok {
		return nil
	}

	var policies gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(route.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list BackendTLSPolicies for GRPCRoute change failed; enqueue skipped, next reconcile from another source recovers",
			"namespace", route.Namespace, "route", route.Name)

		return nil
	}

	requests := make([]reconcile.Request, 0, len(policies.Items))

	for policyIdx := range policies.Items {
		policy := &policies.Items[policyIdx]
		if !grpcRouteReferencesAnyService(route, policyTargetServiceNames(policy), policy.Namespace) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: policy.Namespace, Name: policy.Name},
		})
	}

	return requests
}

// policyTargetServiceNames returns the Service names targeted by a
// policy. Shared by the HTTPRoute and GRPCRoute backend-overlap checks.
func policyTargetServiceNames(policy *gatewayv1.BackendTLSPolicy) map[string]struct{} {
	targets := map[string]struct{}{}

	for _, target := range policy.Spec.TargetRefs {
		kind := string(target.Kind)
		if kind != "" && kind != serviceKind {
			continue
		}

		targets[string(target.Name)] = struct{}{}
	}

	return targets
}
