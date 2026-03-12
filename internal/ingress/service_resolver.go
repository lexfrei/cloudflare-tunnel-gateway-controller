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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
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

// validateBackendGroupKind checks that the backend ref targets a core Service.
// Returns a BackendRefError if the group or kind is not supported.
func validateBackendGroupKind(
	ref gatewayv1.BackendRef,
	namespace, routeName string,
) *BackendRefError {
	if ref.Group != nil && *ref.Group != "" && *ref.Group != backendGroupCoreAlias {
		return &BackendRefError{
			RouteNamespace: namespace,
			RouteName:      routeName,
			BackendName:    string(ref.Name),
			Reason:         string(gatewayv1.RouteReasonInvalidKind),
			Message:        fmt.Sprintf("unsupported backend group %q", *ref.Group),
		}
	}

	if ref.Kind != nil && *ref.Kind != backendKindService {
		return &BackendRefError{
			RouteNamespace: namespace,
			RouteName:      routeName,
			BackendName:    string(ref.Name),
			Reason:         string(gatewayv1.RouteReasonInvalidKind),
			Message:        fmt.Sprintf("unsupported backend kind %q, only Service is supported", *ref.Kind),
		}
	}

	return nil
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
	if validErr := validateBackendGroupKind(ref, namespace, routeName); validErr != nil {
		return "", validErr
	}

	svcNamespace, port := resolveBackendNamespacePort(ref, namespace)

	url, backendErr := resolveServiceURL(ctx, &serviceResolveParams{
		client: resolver.client, validator: resolver.validator,
		logger: resolver.logger, clusterDomain: resolver.clusterDomain,
		routeKind: routeKind, routeNS: namespace, routeName: routeName,
		svcName: string(ref.Name), svcNS: svcNamespace, port: port,
	})

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
		if !validateCrossNamespaceRef(ctx, params.validator, params.logger, params.routeKind, params.routeNS, params.routeName, params.svcNS, params.svcName) {
			return "", &BackendRefError{
				RouteNamespace: params.routeNS,
				RouteName:      params.routeName,
				BackendName:    params.svcName,
				BackendNS:      params.svcNS,
				Reason:         string(gatewayv1.RouteReasonRefNotPermitted),
				Message:        fmt.Sprintf("cross-namespace backend reference to %s/%s not permitted by ReferenceGrant", params.svcNS, params.svcName),
			}
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
