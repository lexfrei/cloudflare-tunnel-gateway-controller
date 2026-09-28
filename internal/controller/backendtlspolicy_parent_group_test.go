package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestParentRefIsGateway_Group pins the BackendTLSPolicy ancestor walk to the
// binding rule: an explicit "" parentRef group is the core group, not ours.
func TestParentRefIsGateway_Group(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		group *gatewayv1.Group
		want  bool
	}{
		{name: "omitted group", want: true},
		{name: "gateway api group", group: new(gatewayv1.Group(gatewayv1.GroupName)), want: true},
		{name: "explicit empty group", group: new(gatewayv1.Group(""))},
		{name: "foreign group", group: new(gatewayv1.Group("example.com"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, parentRefIsGateway(gatewayv1.ParentReference{Group: tt.group, Name: "gw"}))
		})
	}
}
