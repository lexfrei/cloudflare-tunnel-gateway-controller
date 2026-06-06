package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(observedGen int64) metav1.Condition {
	return metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: observedGen,
	}
}

func TestStatusGenerationStale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		reconciledGen int64
		conditionSets [][]metav1.Condition
		want          bool
	}{
		{
			name:          "no conditions",
			reconciledGen: 5,
			conditionSets: nil,
			want:          false,
		},
		{
			name:          "all equal generation is a normal re-write",
			reconciledGen: 5,
			conditionSets: [][]metav1.Condition{{cond(5), cond(5)}},
			want:          false,
		},
		{
			name:          "older stored generation is overwritable",
			reconciledGen: 5,
			conditionSets: [][]metav1.Condition{{cond(3), cond(4)}},
			want:          false,
		},
		{
			name:          "newer stored generation in a single set is stale",
			reconciledGen: 5,
			conditionSets: [][]metav1.Condition{{cond(5), cond(6)}},
			want:          true,
		},
		{
			name:          "newer stored generation in a later set is stale",
			reconciledGen: 5,
			conditionSets: [][]metav1.Condition{{cond(5)}, {cond(4)}, {cond(7)}},
			want:          true,
		},
		{
			name:          "multiple sets all at or below is overwritable",
			reconciledGen: 9,
			conditionSets: [][]metav1.Condition{{cond(9)}, {cond(8)}, {cond(0)}},
			want:          false,
		},
		{
			name:          "empty inner sets are ignored",
			reconciledGen: 1,
			conditionSets: [][]metav1.Condition{{}, nil, {cond(1)}},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := statusGenerationStale(tt.reconciledGen, tt.conditionSets...)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOwnedConditionsStale(t *testing.T) {
	t.Parallel()

	const ownedType = "Accepted"

	stored := func(condType string, gen int64) []metav1.Condition {
		return []metav1.Condition{{Type: condType, Status: metav1.ConditionTrue, ObservedGeneration: gen}}
	}

	tests := []struct {
		name          string
		stored        []metav1.Condition
		reconciledGen int64
		ownedTypes    []string
		want          bool
	}{
		{
			name:          "owned type at newer generation is stale",
			stored:        stored(ownedType, 6),
			reconciledGen: 5,
			ownedTypes:    []string{ownedType},
			want:          true,
		},
		{
			name:          "owned type at equal generation is not stale",
			stored:        stored(ownedType, 5),
			reconciledGen: 5,
			ownedTypes:    []string{ownedType},
			want:          false,
		},
		{
			name:          "owned type at older generation is not stale",
			stored:        stored(ownedType, 3),
			reconciledGen: 5,
			ownedTypes:    []string{ownedType},
			want:          false,
		},
		{
			name:          "newer foreign condition type is ignored",
			stored:        stored("special.io/SomeField", 9),
			reconciledGen: 5,
			ownedTypes:    []string{ownedType},
			want:          false,
		},
		{
			name:          "missing owned type is not stale",
			stored:        nil,
			reconciledGen: 5,
			ownedTypes:    []string{ownedType},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ownedConditionsStale(tt.stored, tt.reconciledGen, tt.ownedTypes...)
			assert.Equal(t, tt.want, got)
		})
	}
}
