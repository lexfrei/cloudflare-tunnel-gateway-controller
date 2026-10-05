package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestApplyGatewayRefusals_RouteServesNowhere pins that a route still bound to
// a dedicated Gateway refused as a whole, as when the refusal lands between
// route binding and the infra listing, is served on no partition: falling
// back to the shared plane would push the tenant's hostnames there.
func TestApplyGatewayRefusals_RouteServesNowhere(t *testing.T) {
	t.Parallel()

	refused := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "infra-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{Addresses: []gatewayv1.GatewaySpecAddress{{Value: "192.0.2.1"}}},
	}

	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolved", true: "broken"}[broken], func(t *testing.T) {
			t.Parallel()

			infra := &infraGateways{
				resolved: map[string]*infraGateway{"default/infra-gw": {}},
				broken:   map[string]bool{"default/infra-gw": broken},
				listed:   []*gatewayv1.Gateway{refused},
			}

			if broken {
				infra.transient = map[string]bool{"default/infra-gw": true}
			}

			applyGatewayRefusals(infra)

			assert.Empty(t, partitionGatewaysFor(routeBindingInfo{acceptedGateways: map[string]bool{"default/infra-gw": true}}, infra))
			assert.False(t, infra.keepsLastPlane("default/infra-gw"), "a refused Gateway's plane is removed, not kept")
			assert.Empty(t, infra.transientKeys(), "a refusal is a decision, not a blip to retry")

			bindings := map[string]routeBindingInfo{"default/route": {parentGateways: map[int]string{0: "default/infra-gw"}}}
			assignParentPartitions(bindings, infra)
			assert.Empty(t, bindings["default/route"].parentPartitions, "the parent is served from no partition")
			assert.Error(t, gatewayPlaneError("default/infra-gw", infra), "the parent's status says why it is not served")
		})
	}
}
