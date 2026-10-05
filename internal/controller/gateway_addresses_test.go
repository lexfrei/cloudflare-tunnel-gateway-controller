package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const addressesTestTunnelHostname = "12345678-1234-1234-1234-123456789abc.cfargotunnel.com"

func reconcileAddressGateway(t *testing.T, addresses ...gatewayv1.GatewaySpecAddress) gatewayv1.Gateway {
	t.Helper()

	return reconcileSelectorGatewayWith(t, gatewayv1.GatewaySpec{
		Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		Addresses: addresses,
	}, nil)
}

func requireGatewayCondition(t *testing.T, gateway *gatewayv1.Gateway, conditionType gatewayv1.GatewayConditionType) metav1.Condition {
	t.Helper()

	condition := findCondition(gateway.Status.Conditions, string(conditionType))
	require.NotNil(t, condition, conditionType)

	return *condition
}

// TestGatewayStatus_SpecAddresses pins how a Gateway reports spec.addresses.
// A tunnel is reachable only at its cfargotunnel.com hostname: a Hostname
// request for that name, or with no value, is served; a Hostname request for
// any other name is accepted but not programmed (AddressNotUsable); any other
// address type is not accepted (UnsupportedAddress).
func TestGatewayStatus_SpecAddresses(t *testing.T) {
	t.Parallel()

	hostname := new(gatewayv1.HostnameAddressType)

	tests := []struct {
		name           string
		addresses      []gatewayv1.GatewaySpecAddress
		wantAccepted   metav1.ConditionStatus
		acceptedReason gatewayv1.GatewayConditionReason
		wantProgrammed metav1.ConditionStatus
		programReason  gatewayv1.GatewayConditionReason
	}{
		{
			name: "no addresses", wantAccepted: metav1.ConditionTrue, acceptedReason: gatewayv1.GatewayReasonAccepted,
			wantProgrammed: metav1.ConditionTrue, programReason: gatewayv1.GatewayReasonProgrammed,
		},
		{
			name:         "the tunnel hostname",
			addresses:    []gatewayv1.GatewaySpecAddress{{Type: hostname, Value: addressesTestTunnelHostname}},
			wantAccepted: metav1.ConditionTrue, acceptedReason: gatewayv1.GatewayReasonAccepted,
			wantProgrammed: metav1.ConditionTrue, programReason: gatewayv1.GatewayReasonProgrammed,
		},
		{
			name:         "a hostname without a value",
			addresses:    []gatewayv1.GatewaySpecAddress{{Type: hostname}},
			wantAccepted: metav1.ConditionTrue, acceptedReason: gatewayv1.GatewayReasonAccepted,
			wantProgrammed: metav1.ConditionTrue, programReason: gatewayv1.GatewayReasonProgrammed,
		},
		{
			name:         "another hostname",
			addresses:    []gatewayv1.GatewaySpecAddress{{Type: hostname, Value: "gateway.example.com"}},
			wantAccepted: metav1.ConditionTrue, acceptedReason: gatewayv1.GatewayReasonAccepted,
			wantProgrammed: metav1.ConditionFalse, programReason: gatewayv1.GatewayReasonAddressNotUsable,
		},
		{
			name:         "an IP address",
			addresses:    []gatewayv1.GatewaySpecAddress{{Type: new(gatewayv1.IPAddressType), Value: "192.0.2.1"}},
			wantAccepted: metav1.ConditionFalse, acceptedReason: gatewayv1.GatewayReasonUnsupportedAddress,
			wantProgrammed: metav1.ConditionFalse, programReason: gatewayv1.GatewayReasonInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			updated := reconcileAddressGateway(t, tt.addresses...)

			accepted := requireGatewayCondition(t, &updated, gatewayv1.GatewayConditionAccepted)
			assert.Equal(t, tt.wantAccepted, accepted.Status)
			assert.Equal(t, string(tt.acceptedReason), accepted.Reason)

			programmed := requireGatewayCondition(t, &updated, gatewayv1.GatewayConditionProgrammed)
			assert.Equal(t, tt.wantProgrammed, programmed.Status)
			assert.Equal(t, string(tt.programReason), programmed.Reason)
		})
	}
}

// TestGatewayStatus_UnusableHostnameIsPrescriptive pins the AddressNotUsable
// message: it names the requested hostname and the one the Gateway is served
// at, and the Gateway keeps advertising the tunnel hostname it still serves.
func TestGatewayStatus_UnusableHostnameIsPrescriptive(t *testing.T) {
	t.Parallel()

	updated := reconcileAddressGateway(t,
		gatewayv1.GatewaySpecAddress{Type: new(gatewayv1.HostnameAddressType), Value: "gateway.example.com"})

	programmed := requireGatewayCondition(t, &updated, gatewayv1.GatewayConditionProgrammed)
	assert.Contains(t, programmed.Message, "gateway.example.com")
	assert.Contains(t, programmed.Message, addressesTestTunnelHostname)

	require.Len(t, updated.Status.Addresses, 1)
	assert.Equal(t, addressesTestTunnelHostname, updated.Status.Addresses[0].Value)
}
