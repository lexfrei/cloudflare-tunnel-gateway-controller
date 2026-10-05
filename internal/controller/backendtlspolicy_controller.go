package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/coregroup"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/parentref"
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
	errBackendTLSCARefUnreadable   = errors.New("BackendTLSPolicy CA ConfigMap could not be read")
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
// (see parentref.InGatewayAPIGroup, Kind "" / "Gateway"). Filters
// non-Gateway parents (ListenerSet, future kinds) out of the BackendTLS
// Policy Ancestor walk so the subsequent Gateway Get does not waste a
// round-trip on a guaranteed 404 — which would have silently dropped
// the entry, masking the leak but leaving noisy reconciles.
func parentRefIsGateway(parentRef gatewayv1.ParentReference) bool {
	if !parentref.InGatewayAPIGroup(parentRef) {
		return false
	}

	if parentRef.Kind != nil && *parentRef.Kind != "" && *parentRef.Kind != kindGateway {
		return false
	}

	return true
}

// BackendTLSPolicyReconciler maintains the status of BackendTLSPolicy
// resources: validates the CA references, computes Accepted and ResolvedRefs
// conditions, and writes them under each affected Gateway as a policy ancestor.
type BackendTLSPolicyReconciler struct {
	client.Client

	Scheme         *runtime.Scheme
	ControllerName string

	// Recorder emits the Events that tell a Gateway it was left out of a full
	// Status.Ancestors. Nil is a no-op (unit tests).
	Recorder events.EventRecorder
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

	gateways, matches, err := r.policyAncestorGateways(ctx, &policy)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to enumerate ancestor gateways")
	}

	var ancestors []policyAncestor

	if len(gateways) > 0 {
		conditions, err := r.computeConditions(ctx, &policy)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to evaluate BackendTLSPolicy conditions")
		}

		ancestors = make([]policyAncestor, 0, len(gateways))

		for idx := range gateways {
			ancestor := policyAncestor{gateway: gateways[idx], conditions: conditions}
			if matches[client.ObjectKeyFromObject(&gateways[idx])] == backendTargetUnsupported {
				ancestor.conditions = targetNotFoundConditions(&policy, conditions)
			}

			ancestors = append(ancestors, ancestor)
		}
	}

	logger.Info("reconciling BackendTLSPolicy",
		"name", policy.Name,
		"namespace", policy.Namespace,
		"gateways", len(gateways),
	)

	if err := r.updateStatus(ctx, req.NamespacedName, ancestors, policy.Generation); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update BackendTLSPolicy status")
	}

	return ctrl.Result{}, nil
}

// computeConditions evaluates the policy spec and returns the two conditions
// to expose (Accepted and ResolvedRefs) per Gateway API semantics.
//
//   - Every CA reference invalid or unresolvable: Accepted=False,
//     Reason=NoValidCACertificate; ResolvedRefs=False, Reason=InvalidCACertificateRef
//     (or InvalidKind for Group/Kind mismatches).
//   - Some CA references invalid: ResolvedRefs=False as above, and Accepted
//     evaluated as if only the valid references were listed — the proxy
//     trusts the valid ones.
//   - Conflict with an older peer policy on at least one shared (Service,
//     SectionName) target: Accepted=False, Reason=Conflicted; ResolvedRefs
//     stays True because the policy's own refs are valid.
//   - All happy: both True. Both DNS-Hostname and URI-type SubjectAltNames
//     are honoured end-to-end by the proxy.
//   - The peers or a CA ConfigMap cannot be read for a reason other than
//     NotFound: no conditions and the error, so Reconcile retries without
//     writing status. A missing CA ConfigMap takes the first case.
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
) ([]metav1.Condition, error) {
	// WellKnownCACertificates is not supported — only explicit CACertificateRefs
	// are honoured. The CRD CEL admits a WellKnown-only policy (empty
	// caCertificateRefs + wellKnownCACertificates set), and the Gateway API spec
	// mandates Accepted=False/Invalid for an unsupported WellKnown value, not the
	// generic NoValidCACertificate that an empty-refs policy would otherwise get.
	if len(policy.Spec.Validation.CACertificateRefs) == 0 && policy.Spec.Validation.WellKnownCACertificates != nil {
		return wellKnownUnsupportedConditions(policy.Generation, *policy.Spec.Validation.WellKnownCACertificates), nil
	}

	valid, invalid, err := r.validateCARefs(ctx, policy)
	if err != nil {
		return nil, err
	}

	if valid == 0 {
		return caInvalidConditions(policy.Generation, errors.Join(invalid...)), nil
	}

	winner, err := r.conflictWinnerFor(ctx, policy)
	if err != nil {
		return nil, err
	}

	conditions := acceptedConditions(policy.Generation)
	if winner != nil {
		conditions = conflictedConditions(policy.Generation, winner)
	}

	if len(invalid) > 0 {
		conditions[1] = caInvalidConditions(policy.Generation, errors.Join(invalid...))[1]
	}

	return conditions, nil
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
// A List error is returned, so the reconcile retries instead of stamping
// a status it could not evaluate.
func (r *BackendTLSPolicyReconciler) conflictWinnerFor(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) (*gatewayv1.BackendTLSPolicy, error) {
	ownTargets := normalizePolicyTargets(policy)

	var list gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &list, client.InNamespace(policy.Namespace)); err != nil {
		return nil, errors.Wrap(err, "listing BackendTLSPolicies for the conflict check")
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

	return winner, nil
}

// normalizePolicyTargets canonicalises a policy's core Service TargetRefs to
// a deduplicated set of (Name, SectionName) keys; other targets are not
// attached and so cannot conflict. SectionName comparison is literal — a
// policy without SectionName covers ALL ports of the Service, but per GEP-713
// it does NOT collide with a separate policy that scopes itself to a specific
// named port (different scopes ⇒ no conflict, both Accepted). At runtime
// selectPolicyForServicePort lets the scoped policy govern its named port.
func normalizePolicyTargets(policy *gatewayv1.BackendTLSPolicy) map[targetKey]struct{} {
	keys := map[targetKey]struct{}{}

	for _, target := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(target.LocalPolicyTargetReference) {
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

// validateCARefs counts the CA references that resolve to a ConfigMap whose
// "ca.crt" key holds at least one parseable PEM CERTIFICATE block, and returns
// an error per invalid reference. Only same-namespace ConfigMap refs are
// supported (Core). A ConfigMap that cannot be read for a reason other than
// NotFound is returned as the last error (wrapping errBackendTLSCARefUnreadable), since
// its validity is unknown.
func (r *BackendTLSPolicyReconciler) validateCARefs(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) (int, []error, error) {
	refs := policy.Spec.Validation.CACertificateRefs
	if len(refs) == 0 {
		return 0, []error{errBackendTLSNoCARef}, nil
	}

	valid := 0

	var invalid []error

	for _, ref := range refs {
		refErr := r.validateCARef(ctx, policy.Namespace, ref)
		if errors.Is(refErr, errBackendTLSCARefUnreadable) {
			return 0, nil, refErr
		}

		if refErr != nil {
			invalid = append(invalid, refErr)

			continue
		}

		valid++
	}

	return valid, invalid, nil
}

func (r *BackendTLSPolicyReconciler) validateCARef(
	ctx context.Context,
	namespace string,
	ref gatewayv1.LocalObjectReference,
) error {
	group := string(ref.Group)
	if !coregroup.Is(group) {
		return fmt.Errorf("%w: %q", errBackendTLSUnsupportedGroup, group)
	}

	if string(ref.Kind) != configMapKind {
		return fmt.Errorf("%w: %q", errBackendTLSUnsupportedKind, ref.Kind)
	}

	key := client.ObjectKey{Namespace: namespace, Name: string(ref.Name)}

	configMap, err := getConfigMap(ctx, r.Client, key)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %w", errBackendTLSCARefUnreadable, err)
		}

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

	return nil
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

// updateStatus replaces this controller's entries in Status.Ancestors with
// the supplied ones; with none, it removes this controller's entries.
// Other controllers' entries are preserved. meta.SetStatusCondition is used to
// merge each condition into the existing ancestor (when present), preserving
// LastTransitionTime unless Status changed. Each Gateway left out because the
// list is full gets a Warning Event.
func (r *BackendTLSPolicyReconciler) updateStatus(
	ctx context.Context,
	policyKey client.ObjectKey,
	ancestors []policyAncestor,
	reconciledGen int64,
) error {
	var (
		dropped []policyAncestor
		fresh   gatewayv1.BackendTLSPolicy
	)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dropped = nil

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
		if stale || len(existing) == 0 && len(ancestors) == 0 {
			return nil
		}

		ourEntries := r.mergeAncestorEntries(existing, ancestors)

		// Reserve the full slot count for other controllers' entries first;
		// only OUR entries get truncated when the combined set exceeds the
		// spec's 16-entry cap. Other controllers' status MUST NOT be dropped
		// by our reconciler.
		available := max(policyAncestorStatusMaxCount-len(otherControllerEntries), 0)

		if len(ourEntries) > available {
			dropped = ancestors[available:]
			ourEntries = ourEntries[:available]
		}

		combined := otherControllerEntries
		combined = append(combined, ourEntries...)
		fresh.Status.Ancestors = combined

		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		return err //nolint:wrapcheck // the caller wraps
	}

	r.warnDroppedAncestors(&fresh, dropped)

	return nil
}

// mergeAncestorEntries builds this controller's PolicyAncestorStatus entries,
// merging each ancestor's conditions into its previous ones.
func (r *BackendTLSPolicyReconciler) mergeAncestorEntries(
	existing map[client.ObjectKey][]metav1.Condition,
	ancestors []policyAncestor,
) []gatewayv1.PolicyAncestorStatus {
	entries := make([]gatewayv1.PolicyAncestorStatus, 0, len(ancestors))

	for idx := range ancestors {
		gateway := &ancestors[idx].gateway
		merged := append([]metav1.Condition(nil), existing[client.ObjectKeyFromObject(gateway)]...)

		for _, condition := range ancestors[idx].conditions {
			meta.SetStatusCondition(&merged, condition)
		}

		entries = append(entries, gatewayv1.PolicyAncestorStatus{
			AncestorRef:    gatewayAncestorRef(gateway),
			ControllerName: gatewayv1.GatewayController(r.ControllerName),
			Conditions:     merged,
		})
	}

	return entries
}

// warnDroppedAncestors tells each Gateway left out of a full Status.Ancestors
// that the policy cannot be represented for it. The Gateway API names no
// condition for this, so it is an Event on the Gateway.
func (r *BackendTLSPolicyReconciler) warnDroppedAncestors(policy *gatewayv1.BackendTLSPolicy, dropped []policyAncestor) {
	if r.Recorder == nil {
		return
	}

	for idx := range dropped {
		r.Recorder.Eventf(&dropped[idx].gateway, policy, corev1.EventTypeWarning,
			eventReasonPolicyAncestorsFull, eventActionRecordPolicyStatus,
			"BackendTLSPolicy %s/%s already lists %d ancestors, the most its status holds; it has no status entry for this Gateway",
			policy.Namespace, policy.Name, policyAncestorStatusMaxCount)
	}
}

const (
	eventReasonPolicyAncestorsFull = "PolicyAncestorsFull"
	eventActionRecordPolicyStatus  = "RecordPolicyStatus"
)

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

		if conditionsStaleBy(reconciledGen, isControllerOwnedPolicyAncestorConditionType, ancestor.Conditions) {
			// Stale: the caller skips the write, so the partial existing/others
			// built so far is meaningless — return nil to make that explicit.
			return nil, nil, true
		}

		key := ancestorRefKey(ancestor.AncestorRef, fresh.Namespace)
		existing[key] = ancestor.Conditions
	}

	return existing, others, false
}

// isControllerOwnedPolicyAncestorConditionType reports whether this controller
// writes a condition type into its own PolicyAncestorStatus entry. Other types
// belong to another controller: PolicyAncestorStatus.Conditions godoc forbids
// changing them, and their observedGeneration is unrelated to ours.
func isControllerOwnedPolicyAncestorConditionType(condType string) bool {
	return condType == string(gatewayv1.PolicyConditionAccepted) ||
		condType == string(gatewayv1.BackendTLSPolicyConditionResolvedRefs)
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
		Recorder:       mgr.GetEventRecorder("backendtlspolicy-controller"),
	}

	if err := backendTLSPolicyReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup BackendTLSPolicy controller")
	}

	return nil
}

// SetupWithManager wires the reconciler with watches for BackendTLSPolicy,
// HTTPRoutes and GRPCRoutes (target membership), ListenerSets (which Gateway
// a route attached to one reaches), ReferenceGrants (whether a
// route in another namespace may use the target), and ConfigMaps (CA bundle
// source).
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
		Watches(
			&gatewayv1.ListenerSet{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForListenerSetChange),
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForReferenceGrantChange),
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
			Namespace: peer.Namespace, Name: peer.Name,
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
			Namespace: policy.Namespace, Name: policy.Name,
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
		if !coregroup.Is(group) {
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

// policiesForRouteChange enqueues the BackendTLSPolicies whose TargetRefs
// name one of the HTTPRoute's backends, in every namespace those backends
// point into. A change to a route that doesn't touch a policy's target should
// not bump that policy's status.
func (r *BackendTLSPolicyReconciler) policiesForRouteChange(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	return r.policiesForBackends(ctx, route.Namespace, httpRouteBackends(route))
}

// policiesForGRPCRouteChange is the GRPCRoute counterpart of
// policiesForRouteChange.
func (r *BackendTLSPolicyReconciler) policiesForGRPCRouteChange(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.GRPCRoute)
	if !ok {
		return nil
	}

	return r.policiesForBackends(ctx, route.Namespace, grpcRouteBackends(route))
}

func (r *BackendTLSPolicyReconciler) policiesForBackends(
	ctx context.Context,
	routeNamespace string,
	backends []gatewayv1.BackendObjectReference,
) []reconcile.Request {
	namespaces := map[string]struct{}{}
	for _, ref := range backends {
		namespaces[backendRefNamespace(ref, routeNamespace)] = struct{}{}
	}

	var requests []reconcile.Request

	for namespace := range namespaces {
		var policies gatewayv1.BackendTLSPolicyList
		if err := r.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
			log.FromContext(ctx).Error(err, "list BackendTLSPolicies for route change failed; enqueue skipped, next reconcile from another source recovers",
				"namespace", namespace)

			continue
		}

		for policyIdx := range policies.Items {
			policy := &policies.Items[policyIdx]
			if policyMatchesAnyBackend(policy, backends, routeNamespace) {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(policy),
				})
			}
		}
	}

	return requests
}

// policiesForListenerSetChange enqueues the policies on the backends of every
// route attached to the ListenerSet: moving it to another Gateway changes the
// policies' ancestors without changing the routes' status.
func (r *BackendTLSPolicyReconciler) policiesForListenerSetChange(ctx context.Context, obj client.Object) []reconcile.Request {
	listenerSet, ok := obj.(*gatewayv1.ListenerSet)
	if !ok {
		return nil
	}

	var httpRoutes gatewayv1.HTTPRouteList

	var grpcRoutes gatewayv1.GRPCRouteList

	if err := r.List(ctx, &httpRoutes); err != nil {
		log.FromContext(ctx).Error(err, "list HTTPRoutes for ListenerSet change failed; enqueue skipped")

		return nil
	}

	if err := r.List(ctx, &grpcRoutes); err != nil {
		log.FromContext(ctx).Error(err, "list GRPCRoutes for ListenerSet change failed; enqueue skipped")

		return nil
	}

	var requests []reconcile.Request

	for idx := range httpRoutes.Items {
		route := &httpRoutes.Items[idx]
		if routeTargetsListenerSet(HTTPRouteWrapper{route}, listenerSet) {
			requests = append(requests, r.policiesForBackends(ctx, route.Namespace, httpRouteBackends(route))...)
		}
	}

	for idx := range grpcRoutes.Items {
		route := &grpcRoutes.Items[idx]
		if routeTargetsListenerSet(GRPCRouteWrapper{route}, listenerSet) {
			requests = append(requests, r.policiesForBackends(ctx, route.Namespace, grpcRouteBackends(route))...)
		}
	}

	return requests
}

// policiesForReferenceGrantChange enqueues every BackendTLSPolicy in the
// grant's namespace: a grant decides whether a route in another namespace may
// use a Service there, and so whether that route's Gateways are ancestors.
func (r *BackendTLSPolicyReconciler) policiesForReferenceGrantChange(ctx context.Context, obj client.Object) []reconcile.Request {
	var policies gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list BackendTLSPolicies for ReferenceGrant change failed; enqueue skipped",
			"namespace", obj.GetNamespace())

		return nil
	}

	requests := make([]reconcile.Request, 0, len(policies.Items))
	for policyIdx := range policies.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&policies.Items[policyIdx]),
		})
	}

	return requests
}
