package ingress

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

const (
	// DefaultBackendWeight is the default weight for backends per Gateway API spec.
	DefaultBackendWeight int32 = 1

	// MinBackendWeight is the minimum valid weight per Gateway API spec.
	// Backends with weight=0 are disabled and should not receive traffic.
	MinBackendWeight int32 = 0

	// MaxBackendWeight is the maximum valid weight per Gateway API spec.
	MaxBackendWeight int32 = 1_000_000
)

// SelectHighestWeightIndex returns the index of the backend with highest weight.
// Backends with weight=0 are skipped (disabled per Gateway API spec).
// If weights are equal, returns the first one for deterministic behavior.
// Returns -1 if slice is empty or all backends have weight=0.
//
// Both route kinds project their refs to plain gatewayv1.BackendRef before
// selection (HTTPBackendRef and GRPCBackendRef embed it), so one concrete
// signature replaces the former per-kind wrapper types.
func SelectHighestWeightIndex(refs []gatewayv1.BackendRef) int {
	if len(refs) == 0 {
		return -1
	}

	selectedIdx := -1

	var highestWeight int32

	for i := range refs {
		weight := effectiveBackendWeight(&refs[i])

		// Skip backends with weight=0 (disabled per Gateway API spec)
		if weight == 0 {
			continue
		}

		if selectedIdx == -1 || weight > highestWeight {
			highestWeight = weight
			selectedIdx = i
		}
	}

	return selectedIdx
}
