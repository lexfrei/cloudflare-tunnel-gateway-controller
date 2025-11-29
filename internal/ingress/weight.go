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

// WeightedRef is an interface for backend references with optional weight.
type WeightedRef interface {
	GetWeight() *int32
}

// SelectHighestWeightIndex returns the index of the backend with highest weight.
// Backends with weight=0 are skipped (disabled per Gateway API spec).
// If weights are equal, returns the first one for deterministic behavior.
// Returns -1 if slice is empty or all backends have weight=0.
func SelectHighestWeightIndex[T WeightedRef](refs []T) int {
	if len(refs) == 0 {
		return -1
	}

	selectedIdx := -1

	var highestWeight int32

	for i := range refs {
		weight := DefaultBackendWeight
		if w := refs[i].GetWeight(); w != nil {
			weight = *w
		}

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

// httpBackendRefWrapper wraps HTTPBackendRef to implement WeightedRef.
type httpBackendRefWrapper struct {
	ref *gatewayv1.HTTPBackendRef
}

func (w httpBackendRefWrapper) GetWeight() *int32 {
	return w.ref.Weight
}

// wrapHTTPBackendRefs wraps a slice of HTTPBackendRef for use with SelectHighestWeightIndex.
func wrapHTTPBackendRefs(refs []gatewayv1.HTTPBackendRef) []httpBackendRefWrapper {
	wrapped := make([]httpBackendRefWrapper, len(refs))
	for i := range refs {
		wrapped[i] = httpBackendRefWrapper{ref: &refs[i]}
	}

	return wrapped
}

// grpcBackendRefWrapper wraps GRPCBackendRef to implement WeightedRef.
type grpcBackendRefWrapper struct {
	ref *gatewayv1.GRPCBackendRef
}

func (w grpcBackendRefWrapper) GetWeight() *int32 {
	return w.ref.Weight
}

// wrapGRPCBackendRefs wraps a slice of GRPCBackendRef for use with SelectHighestWeightIndex.
func wrapGRPCBackendRefs(refs []gatewayv1.GRPCBackendRef) []grpcBackendRefWrapper {
	wrapped := make([]grpcBackendRefWrapper, len(refs))
	for i := range refs {
		wrapped[i] = grpcBackendRefWrapper{ref: &refs[i]}
	}

	return wrapped
}
