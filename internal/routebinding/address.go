package routebinding

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

// ParentUnsupportedAddressMessage is the route and ListenerSet status message
// for a parent Gateway refused by UnsupportedAddressType.
const ParentUnsupportedAddressMessage = "The parent Gateway is not accepted: its spec.addresses requests " +
	"an address type other than Hostname, which a Cloudflare Tunnel cannot serve"

// UnsupportedAddressType returns the first spec.addresses type a Gateway
// requests that is not Hostname. A tunnel is reachable only at its
// cfargotunnel.com hostname, so no Gateway requesting another type is accepted
// and no route binds to one. An omitted type is IPAddress, the API default.
func UnsupportedAddressType(gateway *gatewayv1.Gateway) (gatewayv1.AddressType, bool) {
	for _, address := range gateway.Spec.Addresses {
		addressType := gatewayv1.IPAddressType
		if address.Type != nil {
			addressType = *address.Type
		}

		if addressType != gatewayv1.HostnameAddressType {
			return addressType, true
		}
	}

	return "", false
}

// GatewayRefused returns the route and ListenerSet status message for a Gateway
// refused as a whole, which admits no route or ListenerSet and runs no plane.
func GatewayRefused(gateway *gatewayv1.Gateway) (string, bool) {
	if RequestsFrontendValidation(gateway) {
		return ParentRequestsFrontendValidationMessage, true
	}

	if _, unsupported := UnsupportedAddressType(gateway); unsupported {
		return ParentUnsupportedAddressMessage, true
	}

	return "", false
}
