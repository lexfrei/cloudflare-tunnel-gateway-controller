# Gateway type — SHOULD / SHOULD NOT and MAY tier audit

Gateway API v1.5.1, Gateway (`gateway_types.go`) clauses. Verified against actual code: `internal/controller/gateway_controller.go`, `internal/controller/listenerset_view.go`, `internal/listenermerge/merge.go`, `internal/config/resolver.go`, `internal/proxy/router.go`, `internal/routebinding/`. Architecture: status-only Gateway reconciler (v3), single in-process L7 proxy embedded in cloudflared (`OverrideProxy`), all tunnel traffic bypasses cloudflared native ingress; edge terminates TLS; no static IPs, no multi-port, single ingress flattening listeners.

## TASK A — SHOULD / SHOULD NOT

| ID | Keyword | Verdict | Evidence | Notes |
| --- | --- | --- | --- | --- |
| GW-12 | SHOULD NOT | HONOURED-TESTED | `gateway_controller.go:816-840` buildListenerAcceptedCondition; test `gateway_listener_protocol_test.go:87` TestGatewayReconciler_UnsupportedListenerProtocol_AcceptedFalse (TCP case) | TCP/TLS/UDP listeners set Accepted=False / UnsupportedProtocol, not accepted. |
| GW-22 | SHOULD | HONOURED-TESTED | `listenerset_view.go:59` gatewayConflictedListenersMessage (joins conflicted names); test `gateway_controller_test.go:775` asserts message contains "http2" | ListenersNotValid message names the conflicted listeners. |
| GW-23 | SHOULD | HONOURED-TESTED | `gateway_controller.go:319-323` + `listenerset_view.go:90` conflictedGatewayListenerConditions sets per-listener Conflicted=True; ListenerSet side `listenerset_protocol_test.go` / `listenerset_view_test.go:31` | Per-listener status indicates which are conflicted/not Accepted. |
| GW-24 | SHOULD | HONOURED-TESTED | `proxy/router.go:174-200` Route(): exact host then wildcard then default, returns first match; test `proxy/integration_test.go:2007` TestHandler_WildcardHostnameRouting | A request resolves to at most one bucket. |
| GW-25 | SHOULD | HONOURED-TESTED | `proxy/router.go:178-184` exactHosts tried before wildcardHosts; test `proxy/integration_test.go:2041` "exact host wins over wildcard" subtest | foo.example.com routed via exact listener over /*.example.com. |
| GW-28 | SHOULD | N-A | `conformance_test.go:140,278` SupportGatewayHTTPListenerIsolation in ExemptFeatures + GatewayHTTPListenerIsolation in SkipTests; `limitations.md:101` (HTTPRouteHostnameIntersection skip rationale, `conformance_test.go:220-231`) | Listener Isolation NOT supported (single flattened ingress); clause only binds implementations that DO support it. Correctly does not claim the feature (satisfies the paired GW-27 MUST NOT). |
| GW-38 | SHOULD | N-A | `conformance_test.go:145` SupportGatewayHTTPSListenerDetectMisdirectedRequests exempt; `limitations.md:98` tls Ignored — Cloudflare manages TLS | Edge terminates TLS; controller never sees SNI. No HTTPS listener data plane in cluster. |
| GW-40 | SHOULD | N-A | Same as GW-38 — `conformance_test.go:145,235` HTTPRouteHTTPSListenerDetectMisdirectedRequests skipped; `limitations.md:98` | Misdirected-request detection requires SNI/authority comparison at the TLS terminator (the edge), not the in-cluster proxy. |
| GW-41 | SHOULD | N-A | Same as GW-40; no `421`/StatusMisdirected anywhere in `internal/` | 421 on more-specific listener match is an HTTPS-listener-isolation concern; edge-terminated, no isolation. |
| GW-42 | SHOULD | N-A | Same as GW-41 | 421 on non-matching Host — same edge-TLS / no-isolation reason. |
| GW-74 | SHOULD | DEVIATED-DOCUMENTED | `gateway_controller.go:220-225` always writes the tunnel CNAME to status.addresses and never reads spec.addresses; `limitations.md:103-105` "`spec.addresses` is not honoured" | Address is auto-assigned (tunnel CNAME) regardless of requested type; user-requested type is ignored, documented as the GatewayStaticAddresses constraint. The auto-assign half of the clause IS honoured; the "matching the requested type" half is the documented deviation. |
| GW-82 | SHOULD | N-A | No `spec.infrastructure` handling in `internal/`/`api/`/`cmd/`; `conformance_test.go:141,279` SupportGatewayInfrastructurePropagation exempt + GatewayInfrastructure skipped | Controller creates NO resources in response to a Gateway (status-only; proxy lifecycle owned by Helm chart), so there is nothing to apply infra labels to. |
| GW-83 | SHOULD | N-A | Same as GW-82 | No label-to-Pod mapping exists (no Gateway-owned resources); nothing to warn about. |
| GW-84 | SHOULD | N-A | Same as GW-82 | No Gateway-owned resources to annotate. |
| GW-85 | SHOULD | HONOURED-TESTED | `gateway_controller.go:172-181,345-397` setConfigErrorStatus → Accepted=False / InvalidParameters when GatewayClassConfig is unresolvable; `config/resolver.go:91-127` rejects missing/wrong-group/wrong-kind parametersRef; tests `gateway_controller_test.go:1207` (Reason=InvalidParameters), `:2245`, `:3344` | Covers referent-not-found, unsupported kind, and malformed-data (missing tunnelID / missing secret key) paths. |
| GW-86 | SHOULD | N-A | Reason AddressNotAssigned is never emitted; address is always assignable (tunnel CNAME, `gateway_controller.go:220`) | The condition this clause governs is unreachable — addresses are auto-assigned and cannot fail to assign. Tied to GatewayStaticAddresses (exempt). |
| GW-87 | SHOULD | N-A | Reason AddressNotUsable is never emitted; spec.addresses is ignored (`limitations.md:103`), so no user address can be unusable | Same family as GW-86 — clause governs a condition the implementation never sets. |
| GW-104 | SHOULD | N-A | OverlappingTLSConfig hostname/cert detection not implemented; HTTPS listeners have no in-cluster data plane (`limitations.md:98`, GW-38) | Clause only binds a controller that supports BOTH overlap reasons; this controller supports neither (TLS edge-terminated). Paired MUST GW-101 (detect overlapping hostnames) is outside this SHOULD/MAY scope but is itself a separate audit item. |

## TASK B — MAY (inventory)

| ID | Verdict | Evidence | Notes |
| --- | --- | --- | --- |
| GW-04 | OMITTED-INTENTIONAL | `gateway_controller.go:99-116` status-only reconciler; no multi-Gateway data-plane merge | One proxy data plane is shared chart-wide, but config is not "merged from multiple Gateways onto one data plane" in the spec sense — each Gateway's routes are synced independently. Tunnel architecture. |
| GW-17 | OMITTED-INTENTIONAL | `gateway_controller.go:238-246` marks whole Gateway ListenersNotValid on any conflict; does not accept a partial conflict-free subset | Conflict handling rejects at Gateway level rather than accepting the partial set — the permitted MAY alternative is declined. |
| GW-29 | OMITTED-INTENTIONAL | No address merging; each Gateway gets its own tunnel-CNAME address (`gateway_controller.go:220`) | Tunnel architecture: no shared address pool. |
| GW-32 | IMPLEMENTED | `gateway_controller.go:220-225` assigns the tunnel CNAME when no addresses specified; `conformance_test.go:65` SupportGatewayAddressEmpty | Implementation-specific auto-assignment of an address — exactly this MAY. |
| GW-46 | IMPLEMENTED | `routebinding/binding.go:152-164` (namespace), `routebinding/kind.go:24-41` (kind) honour listener.AllowedRoutes | Namespace (Same/All/Selector) + route-kind filtering enforced. |
| GW-50 | IMPLEMENTED | `proxy/startup_protocol.go`, `proxy/grpc_converter.go`, `conformance_test.go:81` SupportHTTPRouteBackendProtocolH2C | Proxy serves cleartext HTTP/1.1 and HTTP/2 (h2c) upstream; gRPC over h2c supported. |
| GW-56 | OMITTED-INTENTIONAL | Backend client cert validated for PEM well-formedness only (`gateway_controller.go:loadGatewayClientCertPEM` path); no deep content validation | "further validation of certificate content" is optional and declined. |
| GW-58 | OMITTED-INTENTIONAL | TLS edge-terminated; frontend cert refs validated for status but not used for termination (`limitations.md:98`) | Multiple-cert attachment is implementation-specific and N/A to an edge-terminated tunnel. |
| GW-61 | OMITTED-INTENTIONAL | No ListenerTLSConfig.Options consumed | Future-API-extension MAY; nothing to implement today. |
| GW-63 | OMITTED-INTENTIONAL | FrontendTLSValidation exempt (`conformance_test.go:143-144` SupportGatewayFrontendClientCertificateValidation in ExemptFeatures) | Frontend client-cert validation not supported (edge-terminated). |
| GW-68 | OMITTED-INTENTIONAL | Same as GW-63 | Multiple CA cert attachment is implementation-specific; frontend validation unsupported. |
| GW-102 | OMITTED-INTENTIONAL | OverlappingTLSConfig overlapping-certificate detection not implemented (no HTTPS data plane) | The MAY half of the GW-101/102 pair; declined alongside the unsupported HTTPS listener surface. |

## Cross-checks against rows-GW.md

The first-pass `rows-GW.md` 2nd column is a Field column (mis-labelled), not a Keyword column, as warned — keywords were re-derived independently from `01-clause-inventory.md` and confirmed against `gateway_types.go` line refs. No keyword-classification disagreements were found for the 18 SHOULD/SHOULD NOT + 12 MAY rows once the inventory (not rows-GW.md) was used as the source.
