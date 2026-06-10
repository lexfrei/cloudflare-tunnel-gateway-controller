package proxy

import (
	"context"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// backendRefOutcome tells the per-kind converter what the shared backendRef
// resolution decided.
type backendRefOutcome int

const (
	// backendRefResolved: the backend is valid; backend carries Weight and
	// URL, and the per-kind transport/filter handling should run.
	backendRefResolved backendRefOutcome = iota
	// backendRefFinal: resolution finished early (invalid ref marked with its
	// 500 fraction, or dropped at zero weight); return (backend, keep) as-is.
	backendRefFinal
	// backendRefDropped: skip the backend entirely (negative weight).
	backendRefDropped
	// backendRefExternal: the ref targets an ExternalBackend; delegate to the
	// per-kind external path (URL comes from the CRD, port is ignored).
	backendRefExternal
)

// resolvedBackendRef is the result of the shared backendRef resolution.
type resolvedBackendRef struct {
	outcome backendRefOutcome
	backend BackendRef
	keep    bool // meaningful for backendRefFinal
	// Kind-agnostic identity for the per-kind transport steps.
	serviceName  string
	svcNamespace string
	port         int32
}

// resolveCommonBackendRef performs the kind-agnostic half of backendRef
// conversion shared by HTTPRoute and GRPCRoute: weight baseline, negative
// weight drop, supported-kind / port / ReferenceGrant validation (each
// marking the backend 500 for its traffic fraction per the Gateway API spec
// rather than silently dropping it), ExternalBackend detection, and service
// URL construction. Per-kind transport defaults and filters stay with the
// callers.
func resolveCommonBackendRef(
	ctx context.Context,
	ref *gatewayv1.BackendRef,
	routeNamespace string,
	clusterDomain string,
	validator BackendRefValidator,
) resolvedBackendRef {
	result := BackendRef{Weight: 1}
	if ref.Weight != nil {
		result.Weight = *ref.Weight
	}

	if result.Weight < 0 {
		slog.Warn("skipping backend with negative weight",
			"name", string(ref.Name),
			"weight", result.Weight,
		)

		return resolvedBackendRef{outcome: backendRefDropped}
	}

	serviceName := string(ref.Name)

	port := int32(defaultServicePort)
	if ref.Port != nil {
		port = *ref.Port
	}

	svcNamespace := routeNamespace
	if ref.Namespace != nil {
		svcNamespace = string(*ref.Namespace)
	}

	resolved := resolvedBackendRef{
		backend:      result,
		serviceName:  serviceName,
		svcNamespace: svcNamespace,
		port:         port,
	}

	return validateCommonBackendRef(ctx, &resolved, ref.BackendObjectReference, routeNamespace, clusterDomain, validator)
}

// validateCommonBackendRef runs the shared validity gates over an
// identity-resolved backendRef. An invalid backendRef MUST return 500 for its
// traffic fraction per the Gateway API spec, not be silently dropped (which
// would hand its share to the valid siblings): a weight>0 invalid ref stays
// in the weighted pool marked 500, a weight-0 ref carries no traffic and is
// dropped — markInvalidBackend enforces that.
func validateCommonBackendRef(
	ctx context.Context,
	resolved *resolvedBackendRef,
	objRef gatewayv1.BackendObjectReference,
	routeNamespace string,
	clusterDomain string,
	validator BackendRefValidator,
) resolvedBackendRef {
	markInvalid := func(reason string) resolvedBackendRef {
		resolved.outcome = backendRefFinal
		resolved.backend, resolved.keep = markInvalidBackend(
			resolved.backend.Weight, resolved.serviceName, resolved.svcNamespace, resolved.port, clusterDomain, reason)

		return *resolved
	}

	if !IsSupportedBackendRef(objRef) {
		return markInvalid("unsupported backend kind")
	}

	// An ExternalBackend's URL lives in its spec (resolved controller-side);
	// its backendRef port is ignored in favour of spec.port, so skip port
	// validation and let the per-kind external path take over.
	if IsExternalBackendRef(objRef) {
		resolved.outcome = backendRefExternal

		return *resolved
	}

	if !validatePort(resolved.port) {
		return markInvalid("invalid port")
	}

	if !validateCrossNamespace(ctx, resolved.svcNamespace, routeNamespace, resolved.serviceName, objRef, validator) {
		return markInvalid("cross-namespace reference not permitted by ReferenceGrant")
	}

	resolved.outcome = backendRefResolved
	resolved.backend.URL = buildServiceURL(
		resolved.serviceName, resolved.svcNamespace, resolved.port, backendDomain(objRef, clusterDomain))

	return *resolved
}

// routeKindView gives convertRoutesGeneric per-kind access to a route type:
// its hostnames, its parentRefs (for the first-parent client-cert walk), and
// the per-rule conversion. Everything else — precedence sorting, config
// versioning, diagnostics sink wiring — is shared and cannot drift between
// route kinds.
type routeKindView[R metav1.Object] struct {
	hostnames   func(route R) []gatewayv1.Hostname
	parentRefs  func(route R) []gatewayv1.ParentReference
	ruleCount   func(route R) int
	convertRule func(ctx context.Context, route R, ruleIdx int, hostnames []string, clientCert *ClientCertConfig, sink *diagSink) RouteRule
}

// convertRoutesGeneric is the shared conversion shell behind ConvertHTTPRoutes
// and ConvertGRPCRoutes: routes are flattened in spec precedence order (oldest
// creationTimestamp, then namespace/name) so the router's stable ruleIndex
// tiebreak resolves cross-Route ties exactly as the spec mandates, each
// route's first managed parent contributes the backend mTLS client cert, and
// every rule's diagnostics land in one sink.
func convertRoutesGeneric[R metav1.Object](
	ctx context.Context,
	routes []R,
	gatewayCertResolver GatewayClientCertResolver,
	view routeKindView[R],
) *Config {
	cfg := &Config{
		Version: configVersionCounter.Add(1),
	}

	sink := &diagSink{}

	for _, route := range sortRoutesByPrecedence(routes) {
		sink.route(route.GetNamespace(), route.GetName())
		hostnames := convertHostnames(view.hostnames(route))
		clientCert := resolveFirstParentClientCertFromRefs(ctx, view.parentRefs(route), route.GetNamespace(), gatewayCertResolver)

		for ruleIdx := range view.ruleCount(route) {
			sink.at(ruleIdx)
			cfg.Rules = append(cfg.Rules, view.convertRule(ctx, route, ruleIdx, hostnames, clientCert, sink))
		}
	}

	cfg.Diagnostics = sink.items

	return cfg
}
