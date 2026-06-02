package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// backendTargetKind classifies a validated backend ref so the resolver can pick
// the right resolution path.
type backendTargetKind int

const (
	backendTargetService backendTargetKind = iota
	backendTargetServiceImport
	backendTargetExternal
)

// serviceResolveParams contains all parameters needed to resolve a backend service URL.
type serviceResolveParams struct {
	client        client.Reader
	validator     *referencegrant.Validator
	logger        *slog.Logger
	clusterDomain string
	routeKind     string
	routeNS       string
	routeName     string
	svcName       string
	svcNS         string
	port          int
}

// validateBackendGroupKind classifies a backend ref as a core Service or a
// multicluster.x-k8s.io ServiceImport. Returns a BackendRefError if the
// group/kind combination is not a supported backend kind.
func validateBackendGroupKind(
	ref gatewayv1.BackendRef,
	namespace, routeName string,
) (backendTargetKind, *BackendRefError) {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}

	kind := backendKindService
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	switch {
	case (group == backendGroupCore || group == backendGroupCoreAlias) && kind == backendKindService:
		return backendTargetService, nil
	case group == backendGroupServiceImport && kind == backendKindServiceImport:
		return backendTargetServiceImport, nil
	case group == backendGroupExternal && kind == backendKindExternal:
		return backendTargetExternal, nil
	default:
		return backendTargetService, &BackendRefError{
			RouteNamespace: namespace,
			RouteName:      routeName,
			BackendName:    string(ref.Name),
			Reason:         string(gatewayv1.RouteReasonInvalidKind),
			Message: fmt.Sprintf(
				"unsupported backend %s/%s, only Service, ServiceImport and ExternalBackend are supported", group, kind),
		}
	}
}

// resolveBackendNamespacePort extracts the target namespace and port from a BackendRef.
func resolveBackendNamespacePort(ref gatewayv1.BackendRef, routeNamespace string) (string, int) {
	svcNamespace := routeNamespace
	if ref.Namespace != nil {
		svcNamespace = string(*ref.Namespace)
	}

	port := DefaultHTTPPort
	if ref.Port != nil {
		port = int(*ref.Port)
	}

	return svcNamespace, port
}

// resolveValidatedBackend validates a BackendRef and resolves it to a service URL.
// It handles group/kind validation, namespace/port extraction, service resolution, and metrics recording.
func resolveValidatedBackend(
	ctx context.Context,
	resolver *backendResolver,
	ref gatewayv1.BackendRef,
	namespace, routeName, routeKind string,
) (string, *BackendRefError) {
	svcNamespace, port := resolveBackendNamespacePort(ref, namespace)

	// port originates from gatewayv1.PortNumber (int32) or DefaultHTTPPort, so
	// it is always in [1,65535] and the conversion cannot overflow.
	portI32 := int32(port) //nolint:gosec // port is a Gateway PortNumber, never overflows int32

	targetKind, validErr := validateBackendGroupKind(ref, namespace, routeName)
	if validErr != nil {
		validErr.Port = portI32

		return "", validErr
	}

	params := &serviceResolveParams{
		client: resolver.client, validator: resolver.validator,
		logger: resolver.logger, clusterDomain: resolver.clusterDomain,
		routeKind: routeKind, routeNS: namespace, routeName: routeName,
		svcName: string(ref.Name), svcNS: svcNamespace, port: port,
	}

	var (
		url        string
		backendErr *BackendRefError
	)

	switch targetKind {
	case backendTargetServiceImport:
		url, backendErr = resolveServiceImportURL(ctx, params)
	case backendTargetExternal:
		url, backendErr = resolveExternalBackendURL(ctx, params)
	case backendTargetService:
		url, backendErr = resolveServiceURL(ctx, params)
	}

	if backendErr != nil {
		backendErr.Port = portI32

		if targetKind == backendTargetServiceImport {
			// The proxy synthesizes the ServiceImport URL under the clusterset
			// domain; tell the controller so it matches the right proxy backend.
			backendErr.Domain = clustersetDomain
		}
	}

	if resolver.metrics != nil {
		kind := strings.ToLower(strings.TrimSuffix(routeKind, "Route"))

		if backendErr != nil {
			resolver.metrics.RecordBackendRefValidation(ctx, kind, "failed", backendErr.Reason)
		} else {
			resolver.metrics.RecordBackendRefValidation(ctx, kind, "success", "")
		}
	}

	return url, backendErr
}

// resolveServiceURL resolves a backend service reference to a URL.
// It handles ExternalName services, cross-namespace validation, and cluster-local DNS fallback.
func resolveServiceURL(ctx context.Context, params *serviceResolveParams) (string, *BackendRefError) {
	// Validate cross-namespace references with ReferenceGrant
	if params.routeNS != params.svcNS {
		if !validateCrossNamespaceRef(ctx, params.validator, params.logger, params.routeKind, params.routeNS, params.routeName, params.svcNS, params.svcName, backendGroupCore, backendKindService) {
			return "", crossNamespaceDeniedError(params)
		}
	}

	scheme := schemeHTTP
	if params.port == DefaultHTTPSPort {
		scheme = schemeHTTPS
	}

	// Fetch Service to check for ExternalName type
	if params.client != nil {
		svc := &corev1.Service{}

		err := params.client.Get(ctx, types.NamespacedName{
			Name:      params.svcName,
			Namespace: params.svcNS,
		}, svc)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", &BackendRefError{
					RouteNamespace: params.routeNS,
					RouteName:      params.routeName,
					BackendName:    params.svcName,
					BackendNS:      params.svcNS,
					Reason:         string(gatewayv1.RouteReasonBackendNotFound),
					Message:        fmt.Sprintf("Service %s/%s not found", params.svcNS, params.svcName),
				}
			}
			// Log error and fall back to cluster-local DNS
			params.logger.Warn("failed to fetch Service, using cluster-local DNS",
				"service", fmt.Sprintf("%s/%s", params.svcNS, params.svcName),
				"error", err.Error(),
			)
		} else if svc.Spec.Type == corev1.ServiceTypeExternalName {
			return fmt.Sprintf("%s://%s:%d", scheme, svc.Spec.ExternalName, params.port), nil
		}
	}

	return fmt.Sprintf("%s://%s.%s.svc.%s:%d",
		scheme,
		params.svcName,
		params.svcNS,
		params.clusterDomain,
		params.port,
	), nil
}

// crossNamespaceDeniedError builds the RefNotPermitted error for a
// cross-namespace backendRef that no ReferenceGrant authorizes.
func crossNamespaceDeniedError(params *serviceResolveParams) *BackendRefError {
	return &BackendRefError{
		RouteNamespace: params.routeNS,
		RouteName:      params.routeName,
		BackendName:    params.svcName,
		BackendNS:      params.svcNS,
		Reason:         string(gatewayv1.RouteReasonRefNotPermitted),
		Message:        fmt.Sprintf("cross-namespace backend reference to %s/%s not permitted by ReferenceGrant", params.svcNS, params.svcName),
	}
}

// resolveServiceImportURL resolves a multicluster ServiceImport backendRef to a
// clusterset.local URL. It validates a cross-namespace ref against a
// ServiceImport-keyed ReferenceGrant, fetches the ServiceImport to confirm it
// exists and exports the requested port, and returns BackendNotFound otherwise.
// With a nil client (validation disabled) it builds the URL without the lookup.
func resolveServiceImportURL(ctx context.Context, params *serviceResolveParams) (string, *BackendRefError) {
	if params.routeNS != params.svcNS {
		if !validateCrossNamespaceRef(ctx, params.validator, params.logger, params.routeKind, params.routeNS, params.routeName, params.svcNS, params.svcName, backendGroupServiceImport, backendKindServiceImport) {
			return "", crossNamespaceDeniedError(params)
		}
	}

	if params.client != nil {
		imported := &mcsv1alpha1.ServiceImport{}

		err := params.client.Get(ctx, types.NamespacedName{Name: params.svcName, Namespace: params.svcNS}, imported)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", serviceImportNotFoundError(params, fmt.Sprintf("ServiceImport %s/%s not found", params.svcNS, params.svcName))
			}
			// Transient error: fall back to building the URL so a flaky API read
			// does not strip an otherwise-valid backend (mirrors resolveServiceURL).
			params.logger.Warn("failed to fetch ServiceImport, using clusterset DNS",
				"serviceimport", fmt.Sprintf("%s/%s", params.svcNS, params.svcName),
				"error", err.Error(),
			)
		} else if !serviceImportExportsPort(imported, params.port) {
			return "", serviceImportNotFoundError(params,
				fmt.Sprintf("ServiceImport %s/%s does not export port %d", params.svcNS, params.svcName, params.port))
		}
	}

	scheme := schemeHTTP
	if params.port == DefaultHTTPSPort {
		scheme = schemeHTTPS
	}

	return fmt.Sprintf("%s://%s.%s.svc.%s:%d", scheme, params.svcName, params.svcNS, clustersetDomain, params.port), nil
}

// serviceImportExportsPort reports whether the ServiceImport advertises the
// requested port in spec.ports.
func serviceImportExportsPort(imported *mcsv1alpha1.ServiceImport, port int) bool {
	for i := range imported.Spec.Ports {
		if int(imported.Spec.Ports[i].Port) == port {
			return true
		}
	}

	return false
}

// serviceImportNotFoundError builds a BackendNotFound error for a ServiceImport
// that is absent or does not export the requested port.
func serviceImportNotFoundError(params *serviceResolveParams, message string) *BackendRefError {
	return &BackendRefError{
		RouteNamespace: params.routeNS,
		RouteName:      params.routeName,
		BackendName:    params.svcName,
		BackendNS:      params.svcNS,
		Reason:         string(gatewayv1.RouteReasonBackendNotFound),
		Message:        message,
	}
}

// resolveExternalBackendURL resolves an ExternalBackend backendRef to its
// declared scheme://host:port/path URL. A cross-namespace ref is validated
// against an ExternalBackend-keyed ReferenceGrant; an absent ExternalBackend is
// reported as BackendNotFound so the route surfaces ResolvedRefs=False. The URL
// is used for the Cloudflare edge rule and route status; the in-cluster proxy
// resolves its own copy via the converter sentinel.
func resolveExternalBackendURL(ctx context.Context, params *serviceResolveParams) (string, *BackendRefError) {
	if params.routeNS != params.svcNS {
		if !validateCrossNamespaceRef(ctx, params.validator, params.logger, params.routeKind, params.routeNS, params.routeName, params.svcNS, params.svcName, backendGroupExternal, backendKindExternal) {
			return "", crossNamespaceDeniedError(params)
		}
	}

	if params.client == nil {
		return "", nil
	}

	external := &v1alpha1.ExternalBackend{}

	err := params.client.Get(ctx, types.NamespacedName{Name: params.svcName, Namespace: params.svcNS}, external)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", &BackendRefError{
				RouteNamespace: params.routeNS,
				RouteName:      params.routeName,
				BackendName:    params.svcName,
				BackendNS:      params.svcNS,
				Reason:         string(gatewayv1.RouteReasonBackendNotFound),
				Message:        fmt.Sprintf("ExternalBackend %s/%s not found", params.svcNS, params.svcName),
			}
		}
		// Transient read error: no DNS fallback exists for an ExternalBackend, so
		// skip the edge rule this round rather than report a false failure; the
		// next reconcile retries.
		params.logger.Warn("failed to fetch ExternalBackend",
			"externalbackend", fmt.Sprintf("%s/%s", params.svcNS, params.svcName),
			"error", err.Error(),
		)

		return "", nil
	}

	// The CRD host pattern permits a colon (for bracketed IPv6), so a bare
	// host:port slips through admission and would dial a URL that fails to parse.
	// Surface it on the route status instead of a green status + silent 500.
	validErr := external.Spec.Validate()
	if validErr != nil {
		return "", &BackendRefError{
			RouteNamespace: params.routeNS,
			RouteName:      params.routeName,
			BackendName:    params.svcName,
			BackendNS:      params.svcNS,
			Reason:         string(gatewayv1.RouteReasonUnsupportedValue),
			Message:        fmt.Sprintf("ExternalBackend %s/%s is malformed: %v", params.svcNS, params.svcName, validErr),
		}
	}

	return external.Spec.URL(), nil
}
