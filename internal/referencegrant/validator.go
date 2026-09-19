package referencegrant

import (
	"context"

	"github.com/cockroachdb/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// Reference represents a reference from one resource to another.
type Reference struct {
	// Group is the API group of the resource (empty string for core group).
	Group string
	// Kind is the kind of the resource.
	Kind string
	// Namespace is the namespace of the resource.
	Namespace string
	// Name is the name of the resource.
	Name string
}

// Validator validates cross-namespace references against ReferenceGrant resources.
type Validator struct {
	client client.Client
}

// NewValidator creates a new ReferenceGrant validator.
func NewValidator(k8sClient client.Client) *Validator {
	return &Validator{
		client: k8sClient,
	}
}

// IsReferenceAllowed checks if a reference from one resource to another is allowed
// based on ReferenceGrant resources.
//
// References within the same namespace are always allowed.
// Cross-namespace references require a ReferenceGrant in the target namespace.
func (v *Validator) IsReferenceAllowed(ctx context.Context, fromRef, toRef Reference) (bool, error) {
	// Same namespace references are always allowed
	if fromRef.Namespace == toRef.Namespace {
		return true, nil
	}

	// Cross-namespace references require a ReferenceGrant in the target namespace
	var grants gatewayv1beta1.ReferenceGrantList

	err := v.client.List(ctx, &grants, client.InNamespace(toRef.Namespace))
	if err != nil {
		return false, errors.Wrap(err, "failed to list ReferenceGrants")
	}

	// Check if any grant allows this reference
	for i := range grants.Items {
		if v.grantAllowsReference(&grants.Items[i], fromRef, toRef) {
			return true, nil
		}
	}

	return false, nil
}

// grantAllowsReference checks if a specific ReferenceGrant allows the reference.
func (v *Validator) grantAllowsReference(grant *gatewayv1beta1.ReferenceGrant, fromRef, toRef Reference) bool {
	// Check if the grant allows references from the source
	fromAllowed := false

	for _, grantFrom := range grant.Spec.From {
		if v.matchesFrom(grantFrom, fromRef) {
			fromAllowed = true

			break
		}
	}

	if !fromAllowed {
		return false
	}

	// Check if the grant allows references to the target
	for _, grantTo := range grant.Spec.To {
		if v.matchesTo(grantTo, toRef) {
			return true
		}
	}

	return false
}

// coreGroupAlias is the spelling the core API group sometimes carries in a
// ReferenceGrant or a backendRef. The canonical spelling is the empty string,
// which is what every comparison here is written against.
const coreGroupAlias = "core"

// normalizeGroup maps the core API group onto its canonical empty-string
// spelling so a grant and a reference that disagree only on spelling still
// match. Named groups are returned unchanged.
func normalizeGroup(group string) string {
	if group == coreGroupAlias {
		return ""
	}

	return group
}

// matchesFrom checks if the ReferenceGrantFrom matches the source reference.
// Group needs no normalization here: both sides are compared as written, and
// the callers only ever ask about a route kind, which lives in the Gateway API
// group rather than the core one.
func (v *Validator) matchesFrom(grantFrom gatewayv1beta1.ReferenceGrantFrom, fromRef Reference) bool {
	// Check group
	if string(grantFrom.Group) != fromRef.Group {
		return false
	}

	// Check kind
	if string(grantFrom.Kind) != fromRef.Kind {
		return false
	}

	// Check namespace
	if string(grantFrom.Namespace) != fromRef.Namespace {
		return false
	}

	return true
}

// matchesTo checks if the ReferenceGrantTo matches the target reference.
func (v *Validator) matchesTo(grantTo gatewayv1beta1.ReferenceGrantTo, toRef Reference) bool {
	// Both sides are normalized: either may spell the core API group "core",
	// and the two spellings name the same group.
	if normalizeGroup(string(grantTo.Group)) != normalizeGroup(toRef.Group) {
		return false
	}

	// Check kind
	if string(grantTo.Kind) != toRef.Kind {
		return false
	}

	// Check name (if specified in grant)
	if grantTo.Name != nil && string(*grantTo.Name) != toRef.Name {
		return false
	}

	return true
}
