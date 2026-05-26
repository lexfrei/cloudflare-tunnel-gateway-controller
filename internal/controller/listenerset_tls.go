package controller

import (
	"context"
	"encoding/pem"
	"fmt"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// listenerEntryRefsCheck holds the resolved-refs verdict for a single
// ListenerSet entry's TLS certificate references.
type listenerEntryRefsCheck struct {
	// Status is True when every referenced Secret exists, has type
	// kubernetes.io/tls with non-empty cert+key data, and (for
	// cross-namespace refs) a ReferenceGrant in the target namespace permits
	// the reference from ListenerSet.
	Status metav1.ConditionStatus

	// Reason carries one of the Gateway API ResolvedRefs reasons
	// (RefNotPermitted, InvalidCertificateRef, …) when Status is False, or
	// ResolvedRefs when True.
	Reason string

	// Message is a human-readable description, suitable for the matching
	// condition's "message" field.
	Message string
}

// resolveListenerEntryRefs validates the TLS certificate references on a
// single ListenerSet entry. An entry without TLS material is trivially
// resolved.
func resolveListenerEntryRefs(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	entry *gatewayv1.ListenerEntry,
) (listenerEntryRefsCheck, error) {
	if entry.TLS == nil || len(entry.TLS.CertificateRefs) == 0 {
		return listenerEntryRefsCheck{
			Status:  metav1.ConditionTrue,
			Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
			Message: msgReferencesResolved,
		}, nil
	}

	for _, ref := range entry.TLS.CertificateRefs {
		check, err := validateListenerSetCertRef(ctx, cli, listenerSet, ref)
		if err != nil {
			return listenerEntryRefsCheck{}, err
		}

		if check.Status == metav1.ConditionFalse {
			return check, nil
		}
	}

	return listenerEntryRefsCheck{
		Status:  metav1.ConditionTrue,
		Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
		Message: msgReferencesResolved,
	}, nil
}

func validateListenerSetCertRef(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	ref gatewayv1.SecretObjectReference,
) (listenerEntryRefsCheck, error) {
	refKind := kindSecret
	if ref.Kind != nil {
		refKind = string(*ref.Kind)
	}

	refGroup := ""
	if ref.Group != nil {
		refGroup = string(*ref.Group)
	}

	if refGroup != "" || refKind != kindSecret {
		return listenerEntryRefsCheck{
			Status:  metav1.ConditionFalse,
			Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
			Message: fmt.Sprintf("Unsupported certificate ref kind: %s/%s", refGroup, refKind),
		}, nil
	}

	refNamespace := listenerSet.Namespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	if refNamespace != listenerSet.Namespace {
		allowed, err := checkListenerSetSecretReferenceGrant(ctx, cli, listenerSet, refNamespace, ref)
		if err != nil {
			return listenerEntryRefsCheck{
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonRefNotPermitted),
				Message: fmt.Sprintf("Failed to check ReferenceGrant: %v", err),
			}, nil
		}

		if !allowed {
			return listenerEntryRefsCheck{
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonRefNotPermitted),
				Message: fmt.Sprintf("Cross-namespace reference to %s/%s not permitted", refNamespace, ref.Name),
			}, nil
		}
	}

	return validateListenerSetSecretExists(ctx, cli, refNamespace, ref)
}

func validateListenerSetSecretExists(
	ctx context.Context,
	cli client.Client,
	namespace string,
	ref gatewayv1.SecretObjectReference,
) (listenerEntryRefsCheck, error) {
	var secret corev1.Secret

	err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: string(ref.Name)}, &secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return listenerEntryRefsCheck{
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
				Message: fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name),
			}, nil
		}

		return listenerEntryRefsCheck{}, errors.Wrap(err, "failed to get secret")
	}

	if secret.Type != corev1.SecretTypeTLS {
		return listenerEntryRefsCheck{
			Status:  metav1.ConditionFalse,
			Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
			Message: fmt.Sprintf("Secret %s/%s is not of type kubernetes.io/tls", namespace, ref.Name),
		}, nil
	}

	certData, hasCert := secret.Data[corev1.TLSCertKey]
	if !hasCert || len(certData) == 0 {
		return listenerEntryRefsCheck{
			Status:  metav1.ConditionFalse,
			Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
			Message: fmt.Sprintf("Secret %s/%s missing tls.crt data", namespace, ref.Name),
		}, nil
	}

	if block, _ := pem.Decode(certData); block == nil {
		return listenerEntryRefsCheck{
			Status:  metav1.ConditionFalse,
			Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
			Message: fmt.Sprintf("Secret %s/%s contains invalid certificate PEM data", namespace, ref.Name),
		}, nil
	}

	return listenerEntryRefsCheck{
		Status:  metav1.ConditionTrue,
		Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
		Message: msgReferencesResolved,
	}, nil
}

// checkListenerSetSecretReferenceGrant returns true when a ReferenceGrant in
// targetNamespace permits the ListenerSet to reference the Secret. Unlike
// Gateway-scoped grants this one MUST have from.Kind == ListenerSet (per
// spec: ReferenceGrants applied to a Gateway are not inherited by child
// ListenerSets).
func checkListenerSetSecretReferenceGrant(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := cli.List(ctx, &grants, client.InNamespace(targetNamespace)); err != nil {
		return false, errors.Wrap(err, "failed to list ReferenceGrants")
	}

	for i := range grants.Items {
		grant := &grants.Items[i]
		if !grantAllowsListenerSet(grant, listenerSet.Namespace) {
			continue
		}

		for _, target := range grant.Spec.To {
			if target.Group != "" || target.Kind != kindSecret {
				continue
			}

			if target.Name == nil || *target.Name == "" || string(*target.Name) == string(ref.Name) {
				return true, nil
			}
		}
	}

	return false, nil
}

func grantAllowsListenerSet(
	grant *gatewayv1beta1.ReferenceGrant,
	listenerSetNamespace string,
) bool {
	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName &&
			from.Kind == kindListenerSet &&
			string(from.Namespace) == listenerSetNamespace {
			return true
		}
	}

	return false
}
