package routebinding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func addressGateway(addresses ...gatewayv1.GatewaySpecAddress) *gatewayv1.Gateway {
	gateway := frontendValidationGateway(nil)
	gateway.Spec.TLS = nil
	gateway.Spec.Addresses = addresses

	return gateway
}

func TestUnsupportedAddressType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		gateway  *gatewayv1.Gateway
		wantType gatewayv1.AddressType
		want     bool
	}{
		{name: "no addresses", gateway: addressGateway(), want: false},
		{
			name:    "hostname with a value",
			gateway: addressGateway(gatewayv1.GatewaySpecAddress{Type: new(gatewayv1.HostnameAddressType), Value: "x.example"}),
			want:    false,
		},
		{
			name:    "hostname without a value",
			gateway: addressGateway(gatewayv1.GatewaySpecAddress{Type: new(gatewayv1.HostnameAddressType)}),
			want:    false,
		},
		{
			name:     "omitted type is IPAddress",
			gateway:  addressGateway(gatewayv1.GatewaySpecAddress{Value: "192.0.2.1"}),
			wantType: gatewayv1.IPAddressType,
			want:     true,
		},
		{
			name: "named address after a hostname",
			gateway: addressGateway(
				gatewayv1.GatewaySpecAddress{Type: new(gatewayv1.HostnameAddressType)},
				gatewayv1.GatewaySpecAddress{Type: new(gatewayv1.NamedAddressType), Value: "pool"},
			),
			wantType: gatewayv1.NamedAddressType,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			addressType, unsupported := UnsupportedAddressType(tt.gateway)
			assert.Equal(t, tt.want, unsupported)
			assert.Equal(t, tt.wantType, addressType)
		})
	}
}

func TestValidateBinding_RefusesGatewayWithUnsupportedAddress(t *testing.T) {
	t.Parallel()

	result, err := NewValidator(setupFakeClient()).ValidateBinding(context.Background(),
		addressGateway(gatewayv1.GatewaySpecAddress{Value: "192.0.2.1"}),
		&RouteInfo{Name: "r", Namespace: "app", Kind: "HTTPRoute"})
	require.NoError(t, err)

	assert.False(t, result.Accepted)
	assert.Empty(t, result.MatchedListeners)
	assert.Equal(t, gatewayv1.RouteReasonNoMatchingParent, result.Reason)
	assert.Equal(t, ParentUnsupportedAddressMessage, result.Message)
}

func TestEvaluateListenerSetAcceptance_RefusesParentWithUnsupportedAddress(t *testing.T) {
	t.Parallel()

	listenerSet := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"}}

	result, err := NewValidator(setupFakeClient()).EvaluateListenerSetAcceptance(context.Background(),
		addressGateway(gatewayv1.GatewaySpecAddress{Value: "192.0.2.1"}), listenerSet)
	require.NoError(t, err)

	assert.False(t, result.Accepted)
	assert.Equal(t, gatewayv1.ListenerSetReasonParentNotAccepted, result.Reason)
	assert.Equal(t, ParentUnsupportedAddressMessage, result.Message)
}
