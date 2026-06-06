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
