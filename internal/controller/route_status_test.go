package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestBuildParentStatus_IncludesPort(t *testing.T) {
	t.Parallel()

	port := gatewayv1.PortNumber(8080)
	ref := gatewayv1.ParentReference{
		Group:       new(gatewayv1.Group),
		Kind:        new(gatewayv1.Kind),
		Name:        "test-gateway",
		Port:        &port,
		SectionName: new(gatewayv1.SectionName),
	}

	status := buildParentStatus(
		ref, "default", "test-controller", 1,
		metav1.Now(), routeBindingInfo{}, 0, nil, nil,
	)

	require.NotNil(t, status.ParentRef.Port)
	assert.Equal(t, port, *status.ParentRef.Port)
}

func TestBuildParentStatus_NilPort(t *testing.T) {
	t.Parallel()

	ref := gatewayv1.ParentReference{
		Name: "test-gateway",
	}

	status := buildParentStatus(
		ref, "default", "test-controller", 1,
		metav1.Now(), routeBindingInfo{}, 0, nil, nil,
	)

	assert.Nil(t, status.ParentRef.Port)
}
