package ingress

import (
	"context"
	"fmt"
	"log/slog"

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
