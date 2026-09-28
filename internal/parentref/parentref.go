// Package parentref holds the one rule for recognising a route parentRef that
// names a Gateway API resource. The controller and the proxy data plane both
// read parentRefs, so the rule lives in a package without controller
// dependencies.
package parentref

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

// InGatewayAPIGroup reports whether a route parentRef names a Gateway API
// resource. ParentReference.Group is the referent's group, with the Gateway API
// group inferred only when unset: an explicit "" is the core group, so it and
// any other group name some other resource even when kind and name match ours.
func InGatewayAPIGroup(ref gatewayv1.ParentReference) bool {
	return ref.Group == nil || *ref.Group == gatewayv1.GroupName
}
