package routebinding

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

// ParentRequestsFrontendValidationMessage is the route and ListenerSet status
// message for a parent Gateway refused by RequestsFrontendValidation.
const ParentRequestsFrontendValidationMessage = "The parent Gateway is not accepted: it sets spec.tls.frontend " +
	"(client certificate validation), which this controller cannot enforce; " +
	"require client certificates at the Cloudflare edge instead"

// RequestsFrontendValidation reports whether a Gateway sets spec.tls.frontend,
// the client-facing TLS settings whose content is client certificate
// validation. Clients complete TLS with the Cloudflare edge, and the tunnel
// carries no client certificate to the proxy, so no Gateway setting it is
// accepted and no route binds to one, even when the block is empty.
func RequestsFrontendValidation(gateway *gatewayv1.Gateway) bool {
	return gateway.Spec.TLS != nil && gateway.Spec.TLS.Frontend != nil
}
