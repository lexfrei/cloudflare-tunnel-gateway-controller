# Exempt/secondary type clause assessment (OTHER-*)

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| OTHER-01 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute; docs/gateway-api/limitations.md:243 | TLSRoute not supported; tunnel is HTTP(S)-only (TLS terminated at edge); no TLSRoute reconciler exists. |
| OTHER-06 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute; docs/gateway-api/limitations.md:243 | TLSRoute hostname-matching N/A; no TLSRoute handler — TLS listeners marked Accepted=False/UnsupportedProtocol. |
| OTHER-09 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute; docs/gateway-api/limitations.md:96 | TLSRoute not supported; TLS listener protocol has no data plane and is rejected (UnsupportedProtocol). |
| OTHER-10 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute | TLSRoute attach validation N/A; no TLSRoute support; TLS listeners rejected at listener status. |
| OTHER-11 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute; docs/gateway-api/limitations.md:243 | TLSRouteRule.Name uniqueness N/A; no TLSRoute reconciler. |
| OTHER-20 | MUST | N/A | NA | test/conformance/conformance_test.go:148 ExemptFeatures SupportTLSRoute; docs/gateway-api/limitations.md:243 | v1alpha2 TLSRoute alias of OTHER-06; TLSRoute not supported. |
| OTHER-23 | MUST | N/A | NA | docs/gateway-api/limitations.md:242 TCPRoute Not supported; internal/tunnel/origin.go:208 ProxyTCP rejects TCP | TCPRouteRule.Name uniqueness N/A; no TCPRoute reconciler; Cloudflare Tunnel is HTTP-focused. |
| OTHER-25 | MUST | N/A | NA | docs/gateway-api/limitations.md:242 TCPRoute Not supported; internal/tunnel/origin.go:21,208 ProxyTCP rejects TCP | TCPRoute backend-rejection N/A; TCP proxying explicitly rejected, no TCPRoute data plane. |
| OTHER-28 | MUST | N/A | NA | test/conformance/conformance_test.go:151 ExemptFeatures SupportUDPRoute; docs/gateway-api/limitations.md:244 | UDPRouteRule.Name uniqueness N/A; no UDPRoute reconciler; no UDP support in tunnels. |
| OTHER-30 | MUST | N/A | NA | test/conformance/conformance_test.go:151 ExemptFeatures SupportUDPRoute; docs/gateway-api/limitations.md:244 | UDPRoute backend-rejection N/A; no UDPRoute data plane; tunnels have no UDP support. |
| OTHER-33 | MUST NOT | ReferenceGrant | MET | internal/referencegrant/validator.go:47-65 grantAllowsReference; internal/referencegrant/validator_test.go | v1alpha2 ReferenceGrant alias; same storage as v1beta1 — Validator denies ungranted cross-namespace refs. |
| OTHER-44 | MUST | GatewayClass | MET | docs/gateway-api/limitations.md; GatewayClass served from v1 storage (gateway-api/apis/v1) | v1beta1 GatewayClass alias of v1; controller is status-only and does not propagate GatewayClass changes to Gateways, so nothing to document. |
| OTHER-45 | SHOULD | GatewayClass | NA | internal/controller/gatewayclass_controller.go (status-only reconciler) | v1beta1 alias; optional finalizer-on-in-use-GatewayClass not implemented (SHOULD); same v1 storage. |
| OTHER-47 | MUST NOT | ReferenceGrant | MET | internal/referencegrant/validator.go:47-65 grantAllowsReference; internal/referencegrant/validator_test.go | v1beta1 ReferenceGrant (GA storage version the controller imports); Validator denies ungranted cross-namespace refs. |
