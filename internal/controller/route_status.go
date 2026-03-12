package controller

import (
	"context"
	"strings"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// routeAccessor provides type-agnostic access to Gateway API route fields
// needed for status updates. The obj field is used for Get/Update calls,
// while the accessor functions read from the populated object.
type routeAccessor struct {
	obj         client.Object
	parentRefs  func() []gatewayv1.ParentReference
	routeStatus func() *gatewayv1.RouteStatus
	generation  func() int64
}

// routeStatusUpdateParams holds the common parameters for updating route status.
type routeStatusUpdateParams struct {
	k8sClient      client.Client
	controllerName string
}

// updateRouteStatusGeneric updates the status of a route with per-parent binding conditions.
// It fetches a fresh copy, builds parent status entries, and writes the update with retry.
func updateRouteStatusGeneric(
	ctx context.Context,
	params routeStatusUpdateParams,
	routeKey types.NamespacedName,
	newAccessor func() routeAccessor,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	// Compute managed class names once outside the retry loop.
	// The set does not change between retries (conflict retries happen in milliseconds).
	classNames, classErr := managedClassNames(ctx, params.k8sClient, params.controllerName)
	if classErr != nil {
		logging.FromContext(ctx).Warn("failed to get managed class names for route status update",
			"error", classErr)

		classNames = nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return updateRouteParentStatuses(
			ctx, params, routeKey, newAccessor(), classNames, bindingInfo, failedRefs, syncErr,
		)
	})

	return errors.Wrap(err, "failed to update route status after retries")
}

// updateRouteParentStatuses fetches a fresh route, builds parent statuses, and writes the update.
func updateRouteParentStatuses(
	ctx context.Context,
	params routeStatusUpdateParams,
	routeKey types.NamespacedName,
	accessor routeAccessor,
	classNames map[string]bool,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	if err := params.k8sClient.Get(ctx, routeKey, accessor.obj); err != nil {
		return errors.Wrap(err, "failed to get fresh route")
	}

	now := metav1.Now()
	routeStatus := accessor.routeStatus()
	routeStatus.Parents = nil

	for refIdx, ref := range accessor.parentRefs() {
		parentStatus := resolveParentRefStatus(
			ctx, params, accessor, ref, refIdx, classNames, now, bindingInfo, failedRefs, syncErr,
		)
		if parentStatus != nil {
			routeStatus.Parents = append(routeStatus.Parents, *parentStatus)
		}
	}

	if err := params.k8sClient.Status().Update(ctx, accessor.obj); err != nil {
		return errors.Wrap(err, "failed to update route status")
	}

	return nil
}

// resolveParentRefStatus builds a RouteParentStatus for a single parentRef,
// or returns nil if the ref doesn't belong to a managed Gateway.
func resolveParentRefStatus(
	ctx context.Context,
	params routeStatusUpdateParams,
	accessor routeAccessor,
	ref gatewayv1.ParentReference,
	refIdx int,
	classNames map[string]bool,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) *gatewayv1.RouteParentStatus {
	if ref.Kind != nil && *ref.Kind != kindGateway {
		return nil
	}

	namespace := accessor.obj.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	var gateway gatewayv1.Gateway
	if err := params.k8sClient.Get(ctx, client.ObjectKey{
		Name:      string(ref.Name),
		Namespace: namespace,
	}, &gateway); err != nil {
		return nil
	}

	if !classNames[string(gateway.Spec.GatewayClassName)] {
		return nil
	}

	parentStatus := buildParentStatus(
		ref, namespace, params.controllerName,
		accessor.generation(), now,
		bindingInfo, refIdx,
		failedRefs, syncErr,
	)

	return &parentStatus
}

// buildParentStatus constructs a RouteParentStatus entry for a single parent ref.
func buildParentStatus(
	ref gatewayv1.ParentReference,
	namespace string,
	controllerName string,
	generation int64,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	refIdx int,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) gatewayv1.RouteParentStatus {
	parentNS := gatewayv1.Namespace(namespace)

	return gatewayv1.RouteParentStatus{
		ParentRef: gatewayv1.ParentReference{
			Group:       ref.Group,
			Kind:        ref.Kind,
			Namespace:   &parentNS,
			Name:        ref.Name,
			Port:        ref.Port,
			SectionName: ref.SectionName,
		},
		ControllerName: gatewayv1.GatewayController(controllerName),
		Conditions: []metav1.Condition{
			buildAcceptedCondition(generation, now, bindingInfo, refIdx, syncErr),
			buildResolvedRefsCondition(generation, now, failedRefs),
		},
	}
}

func buildAcceptedCondition(
	generation int64,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	refIdx int,
	syncErr error,
) metav1.Condition {
	status := metav1.ConditionTrue
	reason := string(gatewayv1.RouteReasonAccepted)
	message := routeAcceptedMessage

	if syncErr != nil {
		status = metav1.ConditionFalse
		reason = string(gatewayv1.RouteReasonPending)
		message = syncErr.Error()
	} else if bindingResult, hasBinding := bindingInfo.bindingResults[refIdx]; hasBinding && !bindingResult.Accepted {
		status = metav1.ConditionFalse
		reason = string(bindingResult.Reason)
		message = bindingResult.Message
	}

	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionAccepted),
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
}

func buildResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	failedRefs []ingress.BackendRefError,
) metav1.Condition {
	status := metav1.ConditionTrue
	reason := string(gatewayv1.RouteReasonResolvedRefs)
	message := resolvedRefsMessage

	if len(failedRefs) > 0 {
		status = metav1.ConditionFalse
		// Use the reason from the first failed ref — Gateway API spec requires
		// specific reasons like InvalidKind, RefNotPermitted, BackendNotFound, etc.
		reason = failedRefs[0].Reason
		if reason == "" {
			reason = string(gatewayv1.RouteReasonRefNotPermitted)
		}

		message = buildFailedRefsMessage(failedRefs)
	}

	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
}

func buildFailedRefsMessage(failedRefs []ingress.BackendRefError) string {
	var msgBuilder strings.Builder

	msgBuilder.WriteString("Backend references not permitted: ")

	for i, failedRef := range failedRefs {
		if i > 0 {
			msgBuilder.WriteString(", ")
		}

		msgBuilder.WriteString(failedRef.BackendNS + "/" + failedRef.BackendName)
	}

	return msgBuilder.String()
}

// routeStatusEntry represents a single route that needs status update.
type routeStatusEntry struct {
	name        string
	namespace   string
	bindingInfo routeBindingInfo
	failedRefs  []ingress.BackendRefError
	update      func(ctx context.Context, bindingInfo routeBindingInfo, failedRefs []ingress.BackendRefError, syncErr error) error
}

// updateRoutesStatus iterates over route entries and updates status for each.
// Returns the first error encountered (for requeue with backoff).
func updateRoutesStatus(
	ctx context.Context,
	logger interface{ Error(msg string, args ...any) },
	entries []routeStatusEntry,
	syncErr error,
) error {
	var firstErr error

	for _, entry := range entries {
		if err := entry.update(ctx, entry.bindingInfo, entry.failedRefs, syncErr); err != nil {
			logger.Error("failed to update route status", "error", err, "route", entry.namespace+"/"+entry.name)

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// filterFailedRefs returns failed backend refs that belong to the specified route.
func filterFailedRefs(allFailedRefs []ingress.BackendRefError, routeNamespace, routeName string) []ingress.BackendRefError {
	var result []ingress.BackendRefError

	for _, failedRef := range allFailedRefs {
		if failedRef.RouteNamespace == routeNamespace && failedRef.RouteName == routeName {
			result = append(result, failedRef)
		}
	}

	return result
}

func newHTTPRouteAccessor() routeAccessor {
	route := &gatewayv1.HTTPRoute{}

	return routeAccessor{
		obj:         route,
		parentRefs:  func() []gatewayv1.ParentReference { return route.Spec.ParentRefs },
		routeStatus: func() *gatewayv1.RouteStatus { return &route.Status.RouteStatus },
		generation:  func() int64 { return route.Generation },
	}
}

func newGRPCRouteAccessor() routeAccessor {
	route := &gatewayv1.GRPCRoute{}

	return routeAccessor{
		obj:         route,
		parentRefs:  func() []gatewayv1.ParentReference { return route.Spec.ParentRefs },
		routeStatus: func() *gatewayv1.RouteStatus { return &route.Status.RouteStatus },
		generation:  func() int64 { return route.Generation },
	}
}
