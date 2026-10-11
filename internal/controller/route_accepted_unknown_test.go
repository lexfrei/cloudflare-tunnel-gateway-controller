package controller

import (
	"testing"

	"github.com/cockroachdb/errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

const unknownTestGateway = "team-a/gw"

// TestBuildAcceptedCondition_RetryableCausesAreUnknown pins the spec's
// polarity for Reason=Pending (RouteConditionAccepted: "Possible reasons for
// this condition to be Unknown are: Pending"): a parent the controller has not
// yet been able to reconcile reports Accepted=Unknown, while a decision that
// stands until someone changes a resource keeps Accepted=False.
func TestBuildAcceptedCondition_RetryableCausesAreUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		infra *infraGateways
		want  metav1.ConditionStatus
	}{
		{
			name: "data plane resolve failed transiently",
			infra: &infraGateways{
				broken:    map[string]bool{unknownTestGateway: true},
				transient: map[string]bool{unknownTestGateway: true},
			},
			want: metav1.ConditionUnknown,
		},
		{
			name: "tunnel claim could not be checked with Cloudflare",
			infra: &infraGateways{
				broken: map[string]bool{unknownTestGateway: true},
				rejected: map[string]tunnelownership.Rejection{unknownTestGateway: {
					TunnelID: proofTokenTunnel, Unproven: true, Proof: tunnelownership.ProofUnknown,
				}},
			},
			want: metav1.ConditionUnknown,
		},
		{
			name:  "data plane config is invalid",
			infra: &infraGateways{broken: map[string]bool{unknownTestGateway: true}},
			want:  metav1.ConditionFalse,
		},
		{
			name: "Cloudflare refuted the tunnel claim",
			infra: &infraGateways{
				broken: map[string]bool{unknownTestGateway: true},
				rejected: map[string]tunnelownership.Rejection{unknownTestGateway: {
					TunnelID: proofTokenTunnel, Unproven: true, Proof: tunnelownership.ProofRefuted,
				}},
			},
			want: metav1.ConditionFalse,
		},
		{
			name: "tunnel claimed by another namespace",
			infra: &infraGateways{
				broken: map[string]bool{unknownTestGateway: true},
				rejected: map[string]tunnelownership.Rejection{unknownTestGateway: {
					TunnelID: proofTokenTunnel,
				}},
			},
			want: metav1.ConditionFalse,
		},
		{
			name: "namespace at its data plane quota",
			infra: &infraGateways{
				broken:    map[string]bool{unknownTestGateway: true},
				overQuota: map[string]int32{unknownTestGateway: 1},
			},
			want: metav1.ConditionFalse,
		},
		{
			name:  "Gateway refused as a whole",
			infra: &infraGateways{refused: map[string]bool{unknownTestGateway: true}},
			want:  metav1.ConditionFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			planeErr := gatewayPlaneError(unknownTestGateway, tt.infra)
			require.Error(t, planeErr)

			binding := routeBindingInfo{
				bindingResults:   map[int]routebinding.BindingResult{0: {Accepted: true}},
				parentGateways:   map[int]string{0: unknownTestGateway},
				syncErrByGateway: map[string]error{unknownTestGateway: planeErr},
			}

			cond := buildAcceptedCondition(1, metav1.Now(), binding, 0, nil, nil)
			assert.Equal(t, tt.want, cond.Status)
			assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason)
			assert.Equal(t, planeErr.Error(), cond.Message)
		})
	}
}

// TestBuildAcceptedCondition_UnevaluatedParentIsUnknown pins that a parent the
// controller could not read is reported as not yet reconciled.
func TestBuildAcceptedCondition_UnevaluatedParentIsUnknown(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{0: unevaluatedParentResult()},
	}

	cond := buildAcceptedCondition(1, metav1.Now(), binding, 0, nil, nil)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason)
}

// TestBuildAcceptedCondition_GlobalSyncErrorPolarity pins the early-error
// path: a sync that failed before partitioning for a reason that says nothing
// about the configuration (an API server read, a Secret read) is retried, so
// every parent is Unknown; a GatewayClassConfig that is invalid stands until
// it is fixed, so every parent is False.
func TestBuildAcceptedCondition_GlobalSyncErrorPolarity(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{0: {Accepted: true}},
		parentGateways: map[int]string{0: unknownTestGateway},
	}

	transient := buildAcceptedCondition(1, metav1.Now(), binding, 0, errors.New("listing GatewayClasses: timeout"), nil)
	assert.Equal(t, metav1.ConditionUnknown, transient.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), transient.Reason)

	invalid := buildAcceptedCondition(1, metav1.Now(), binding, 0,
		config.MarkInvalidParameters(errors.New("credentials Secret not found")), nil)
	assert.Equal(t, metav1.ConditionFalse, invalid.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), invalid.Reason)
}
