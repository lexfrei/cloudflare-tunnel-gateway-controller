package ingress

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// mockWeightedRef is a test implementation of WeightedRef.
type mockWeightedRef struct {
	weight *int32
}

func (m mockWeightedRef) GetWeight() *int32 {
	return m.weight
}

func int32Ptr(i int32) *int32 {
	return &i
}

func TestSelectHighestWeightIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		weights  []*int32
		expected int
	}{
		{
			name:     "empty slice returns -1",
			weights:  []*int32{},
			expected: -1,
		},
		{
			name:     "single backend returns 0",
			weights:  []*int32{nil},
			expected: 0,
		},
		{
			name:     "single backend with explicit weight returns 0",
			weights:  []*int32{int32Ptr(100)},
			expected: 0,
		},
		{
			name:     "highest weight wins",
			weights:  []*int32{int32Ptr(20), int32Ptr(80)},
			expected: 1,
		},
		{
			name:     "equal weights uses first",
			weights:  []*int32{int32Ptr(50), int32Ptr(50)},
			expected: 0,
		},
		{
			name:     "nil weights use default and first wins",
			weights:  []*int32{nil, nil},
			expected: 0,
		},
		{
			name:     "mixed nil and explicit weights",
			weights:  []*int32{nil, int32Ptr(100), int32Ptr(50)},
			expected: 1,
		},
		{
			name:     "zero weight loses to default",
			weights:  []*int32{int32Ptr(0), nil},
			expected: 1,
		},
		{
			name:     "zero weight loses to explicit weight",
			weights:  []*int32{int32Ptr(0), int32Ptr(1)},
			expected: 1,
		},
		{
			name:     "all zero weights returns -1 (all disabled)",
			weights:  []*int32{int32Ptr(0), int32Ptr(0)},
			expected: -1,
		},
		{
			name:     "single backend with weight=0 returns -1 (disabled)",
			weights:  []*int32{int32Ptr(0)},
			expected: -1,
		},
		{
			name:     "three backends with varying weights",
			weights:  []*int32{int32Ptr(10), int32Ptr(30), int32Ptr(20)},
			expected: 1,
		},
		{
			name:     "last backend has highest weight",
			weights:  []*int32{int32Ptr(10), int32Ptr(20), int32Ptr(100)},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refs := make([]mockWeightedRef, len(tt.weights))
			for i, w := range tt.weights {
				refs[i] = mockWeightedRef{weight: w}
			}

			result := SelectHighestWeightIndex(refs)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBackendWeightConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int32(1), DefaultBackendWeight)
	assert.Equal(t, int32(0), MinBackendWeight)
	assert.Equal(t, int32(1_000_000), MaxBackendWeight)
}
