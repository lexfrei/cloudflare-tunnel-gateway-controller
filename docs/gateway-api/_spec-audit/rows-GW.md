# Gateway clause assessment (GW-*)

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| GW-01 | GatewaySpec.Listeners | CRD | NA | vendor/sigs.k8s.io/gateway-api/apis/v1/gateway_types.go kubebuilder:validation:MinItems=1 | CRD enforces at least one Listener; not a runtime job. |
| GW-02 | GatewaySpec.Listeners | CTRL | MET | internal/listenermerge/merge.go:187 annotateConflicts | Indistinct listeners flagged Conflicted so traffic maps to one listener; tunnel flattens to one table. |
| GW-04 | GatewaySpec.Listeners | N/A | NA | gateway_controller.go status-only; single tunnel data plane | MAY-clause; controller does not merge multiple Gateways onto one data plane. |
| GW-05 | GatewaySpec.Listeners | CTRL | MET | internal/listenermerge/merge.go:197-218 (port+protocol+hostname uniqueness) | Conflict detection over (port,protocol,hostname); CRD also enforces unique listener names. |
| GW-06 | GatewaySpec.Listeners | CTRL | PARTIAL | conformance_test.go:45 SupportGateway/SupportHTTPRoute; gateway_controller.go:826 only HTTP/HTTPS Accepted | HTTP/HTTPS Core honoured; HTTPS frontend TLS not terminated (edge does), TCP/TLS/UDP rejected as UnsupportedProtocol. |
| GW-07 | GatewaySpec.Listeners | CTRL | MET | internal/listenermerge/merge.go:197-218 | Same-protocol listeners must differ in another field or are Conflicted. |
| GW-10 | GatewaySpec.Listeners | CTRL | MET | internal/listenermerge/merge.go:204-218 hostnameKey{port,hostname} | Same-protocol set distinguished by (port,hostname); duplicates Conflicted. |
| GW-11 | GatewaySpec.Listeners | N/A | NA | gateway_controller.go:829 TCP -> UnsupportedProtocol | No TCP data plane; TCP listeners never Accepted, so the shared-port TCP-distinctness rule is moot. |
| GW-12 | GatewaySpec.Listeners | CTRL | MET | gateway_controller.go:829-839 buildListenerAcceptedCondition | TCP listeners are Accepted=False/UnsupportedProtocol (not accepted), satisfying SHOULD NOT. |
| GW-13 | GatewaySpec.Listeners | CTRL | PARTIAL | docs/gateway-api/limitations.md:279 (exact>wildcard>default bucket) | Route-level hostname bucketing is most-specific-first; per-listener most-specific selection is N/A since no listener isolation. |
| GW-14 | GatewaySpec.Listeners | CTRL | MET | docs/gateway-api/limitations.md:279 hostname bucket order | Exact before wildcard before fallback at the proxy hostname-bucket layer. |
| GW-16 | GatewaySpec.Listeners | CTRL | MET | gateway_controller.go:319-323 conflictedGatewayListenerConditions; listenermerge/merge.go:206 | Indistinct listeners get Conflicted=True on listener status. |
| GW-17 | GatewaySpec.Listeners | CTRL | MET | listenermerge/merge.go:26 (higher-precedence wins, loser annotated) | Only conflict-free listeners are accepted; conflicted ones excluded from routing. |
| GW-19 | GatewaySpec.Listeners | CTRL | MET | listenermerge/merge.go:187 annotateConflicts walks precedence order | First (highest-precedence) listener keeps the slot; ALL later indistinct ones flagged Conflicted, no arbitrary winner. |
| GW-20 | GatewaySpec.Listeners | CTRL | PARTIAL | gateway_controller.go:242-246 ListenersNotValid on conflict | Conflicted Gateways marked ListenersNotValid, but Gateway not hard-rejected when zero distinct listeners remain (Accepted still derives from conflict msg only). |
| GW-21 | GatewaySpec.Listeners | CTRL | MET | gateway_controller.go:242-246; gatewayConflictedListenersMessage (listenerset_view.go:59) | ListenersNotValid set on Gateway Accepted when conflicted listeners present. |
| GW-22 | GatewaySpec.Listeners | CTRL | MET | listenerset_view.go:82 "Gateway has conflicted listeners: <names>" | Condition message names the conflicted listeners. |
| GW-23 | GatewaySpec.Listeners | CTRL | MET | gateway_controller.go:319-323; listenermerge/merge.go:198-208 | Per-listener Conflicted condition with reason indicates which are conflicted/not Accepted. |
| GW-24 | GatewaySpec.Listeners | CTRL | MET | docs/gateway-api/limitations.md:279 single hostname bucket per request | Each request resolves one hostname bucket, so at most one distinct listener matches. |
| GW-25 | GatewaySpec.Listeners | N/A | NA | conformance_test.go:140 SupportGatewayHTTPListenerIsolation Exempt | Listener isolation not supported; exact-vs-wildcard listener separation is the isolation feature, N/A. |
| GW-26 | GatewaySpec.Listeners | CTRL | MET | docs/gateway-api/limitations.md:89-101; docs/gateway-api/_spec-audit/02-gep-notes.md:40 | Single-routing-table / no-isolation behaviour documented. |
| GW-27 | GatewaySpec.Listeners | CTRL | MET | conformance_test.go:137-140 ExemptFeatures includes SupportGatewayHTTPListenerIsolation | Feature is Exempt, not claimed in SupportedFeatures. |
| GW-28 | GatewaySpec.Listeners | N/A | NA | conformance_test.go:140 (isolation exempt) | SHOULD-clause for implementations that DO support isolation; this one does not. |
| GW-29 | GatewaySpec.Addresses | N/A | NA | gateway_controller.go:220-225 status-only single tunnel CNAME | MAY merge Gateways onto shared addresses; not done. |
| GW-31 | GatewaySpec.Addresses | CTRL | GAP | gateway_controller.go:220-225 always writes tunnel CNAME, never reads spec.addresses | A user-set spec.addresses value is ignored, not validated; no condition flagged for invalid/unavailable requested address. |
| GW-32 | GatewaySpec.Addresses | CTRL | MET | gateway_controller.go:220-225; conformance_test.go:65 SupportGatewayAddressEmpty | No spec.addresses -> implementation assigns the tunnel CNAME automatically. |
| GW-33 | GatewaySpec.Addresses | CTRL | MET | gateway_controller.go:220-225 Status.Addresses = tunnel CNAME | All listeners share the one assigned tunnel address recorded in GatewayStatus.Addresses. |
| GW-34 | Listener.Name | CRD | NA | vendor gateway_types.go Listener.Name (CEL listener-name uniqueness) | CRD/CEL enforces unique listener names within a Gateway. |
| GW-35 | Listener.Hostname | CTRL | PARTIAL | internal/routebinding/hostname.go:14 HostnamesIntersect | HTTP Host matching applied; TLS SNI / HTTPS dual-match are N/A (edge terminates TLS). |
| GW-36 | Listener.Hostname | N/A | NA | conformance_test.go:148 SupportTLSRoute exempt; edge terminates TLS | No SNI matching in cluster; Cloudflare edge does TLS. |
| GW-37 | Listener.Hostname | CTRL | MET | internal/routebinding/hostname.go:14-30; proxy extractHost (limitations.md:279) | Listener Hostname matched against request Host. |
| GW-38 | Listener.Hostname | N/A | NA | edge terminates TLS; conformance_test.go:145 HTTPSListenerDetectMisdirectedRequests exempt | SNI+Host dual-match SHOULD; no in-cluster TLS termination. |
| GW-39 | Listener.Hostname | N/A | NA | edge terminates TLS (no SNI handling in cluster) | SNI/application-protocol hostname verification done by Cloudflare edge, not this controller. |
| GW-40 | Listener.Hostname | N/A | NA | conformance_test.go:145 SupportGatewayHTTPSListenerDetectMisdirectedRequests exempt | Misdirected-request SNI authority match SHOULD; no in-cluster SNI. |
| GW-41 | Listener.Hostname | N/A | NA | conformance_test.go:145 detect-misdirected exempt | 421 on more-specific SNI listener SHOULD; no in-cluster SNI/listener isolation. |
| GW-42 | Listener.Hostname | N/A | NA | conformance_test.go:145 detect-misdirected exempt | 421 SHOULD; no in-cluster SNI handling. |
| GW-43 | Listener.Hostname | CTRL | PARTIAL | docs/gateway-api/limitations.md:279 hostname bucket selection | Proxy returns 404 for an unmatched hostname route, but this is route-not-listener semantics (no listener isolation); not the SNI-driven 404 of the spec. |
| GW-44 | Listener.Hostname | CTRL | MET | internal/routebinding/binding.go:169-170 HostnamesIntersect gate | Route accepted only when listener+route hostnames intersect (NoMatchingListenerHostname otherwise). |
| GW-45 | Listener.TLS | N/A | NA | edge terminates TLS; conformance_test.go:234 HTTPRouteHTTPSListener skipped | Longest-SNI cert selection happens at Cloudflare edge, not in cluster. |
| GW-46 | Listener.AllowedRoutes | CTRL | MET | internal/routebinding/binding.go:173-182 IsNamespaceAllowed + IsRouteKindAllowed | AllowedRoutes namespace+kind filtering applied to route attachment. |
| GW-48 | Listener.AllowedRoutes | CTRL | MET | internal/routebinding/binding.go:160-195 evaluateBinding (hostname, namespace, kind, protocol) | Matching precedence criteria evaluated in order. |
| GW-49 | ProtocolType | CTRL | MET | gateway_controller.go:829-839 ListenerReasonUnsupportedProtocol; gateway_listener_protocol_test.go:87 | Unsupported protocol -> Accepted=False/UnsupportedProtocol on listener. |
| GW-50 | HTTPProtocolType | CTRL | MET | docs/gateway-api/limitations.md:110 (proxy speaks HTTP/2 cleartext) | HTTP/1.1 cleartext served; h2c upstream supported (MAY). |
| GW-51 | HTTPProtocolType | CTRL | MET | docs/gateway-api/limitations.md:110 kubernetes.io/h2c documented | HTTP/2 cleartext backend support documented. |
| GW-52 | GatewayBackendTLS.ClientCertificateRef | CTRL | MET | gateway_client_cert.go:200-230 buildClientCertResolvedRefsCondition; gateway_controller.go:227,258 | Invalid client cert ref -> ResolvedRefs=False/InvalidClientCertificateRef with message. |
| GW-54 | GatewayBackendTLS.ClientCertificateRef | CTRL | MET | gateway_client_cert.go:105-114 grantChecker; checkSecretReferenceGrant (gateway_controller.go:1034) | Cross-namespace cert ref requires ReferenceGrant. |
| GW-55 | GatewayBackendTLS.ClientCertificateRef | CTRL | MET | gateway_client_cert.go:219-221 (RefNotPermitted) ; errGatewayClientCertRefNotPermitted | Disallowed cross-ns ref -> ResolvedRefs=False/RefNotPermitted. |
| GW-56 | GatewayBackendTLS.ClientCertificateRef | CTRL | MET | gateway_client_cert.go:139 tls.X509KeyPair PEM validation | Performs keypair-content validation (MAY). |
| GW-57 | GatewayBackendTLS.ClientCertificateRef | CTRL | MET | gateway_client_cert.go:228-229 (err.Error() as message) ; InvalidClientCertificateRef | Implementation-specific reason+message set on validation failure. |
| GW-58 | ListenerTLSConfig.CertificateRefs | CTRL | MET | gateway_controller.go:858-863 validateTLSCertificateRefs loops all refs | Validates each of multiple cert refs (implementation-specific multi-cert support). |
| GW-59 | ListenerTLSConfig.CertificateRefs | CTRL | MET | gateway_controller.go:901-914 checkSecretReferenceGrant for cross-ns cert refs | Cross-namespace cert ref needs ReferenceGrant. |
| GW-60 | ListenerTLSConfig.CertificateRefs | CTRL | MET | gateway_controller.go:909-913 ListenerReasonRefNotPermitted | Disallowed ref -> listener ResolvedRefs=False/RefNotPermitted. |
| GW-61 | ListenerTLSConfig.Options | N/A | NA | edge terminates TLS; no listener TLS options consumed | MAY future common keys; not actionable. |
| GW-62 | ListenerTLSConfig.Options | CRD | NA | vendor gateway_types.go TLS.Options domain-prefixed key validation | Domain-prefixed-name constraint enforced by CRD on the map key. |
| GW-63 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 SupportGatewayFrontendClientCertificateValidation Exempt | Frontend client-cert validation not supported (edge terminates TLS). |
| GW-64 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | Frontend validation N/A. |
| GW-65 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | Frontend validation N/A. |
| GW-66 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | Frontend validation N/A. |
| GW-67 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | NoValidCACertificate path N/A; no frontend CA refs consumed. |
| GW-68 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | Multi-CA support N/A. |
| GW-69 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | Cross-ns CA ref grant N/A. |
| GW-70 | FrontendTLSValidation.CACertificateRefs | N/A | NA | conformance_test.go:143 frontend-cert-validation exempt | RefNotPermitted on CA refs N/A. |
| GW-71 | AllowValidOnly | N/A | NA | conformance_test.go:143-144 frontend client-cert validation exempt | Client-cert handshake enforcement done at edge / not supported in cluster. |
| GW-72 | AllowedRoutes.Kinds | CTRL | MET | internal/routebinding/kind.go:108-139 FilterSupportedKinds (HTTPRoute/GRPCRoute) | Only protocol-compatible Route kinds (HTTP/gRPC) accepted. |
| GW-73 | AllowedRoutes.Kinds | CTRL | MET | gateway_controller.go:271,991-1010 InvalidRouteKinds on unsupported kind | Unrecognized route kind -> listener ResolvedRefs=False/InvalidRouteKinds. |
| GW-74 | GatewaySpecAddress.Value | CTRL | PARTIAL | gateway_controller.go:220-225 always assigns tunnel CNAME | Empty value auto-assigned (MET for SHOULD assign), but a requested-type mismatch is not honoured/validated. |
| GW-75 | GatewaySpecAddress.Value | CTRL | GAP | gateway_controller.go:220-225 no AddressNotAssigned path | Controller never sets Programmed=False/AddressNotAssigned; it always programs the tunnel CNAME regardless of requested empty/typed value. |
| GW-76 | GatewayStatus.Conditions | CTRL | MET | gateway_controller.go:198-201,335 RetryOnConflict + fresh Get then Status().Update | Read-modify-write cycle on the conditions field. |
| GW-77 | GatewayStatus.Conditions | CTRL | MET | gateway_client_cert.go:169-177 applyGatewayConditions via meta.SetStatusCondition; gateway_controller_test.go:688 PreservesForeignStatusConditions | Merges by type, preserves foreign conditions. |
| GW-78 | GatewayStatus.Conditions | CTRL | MET | gateway_client_cert.go:169-177 meta.SetStatusCondition (type-scoped) ; test :688 | Foreign special.io/* condition left untouched. |
| GW-79 | GatewayStatus.Conditions | CTRL | MET | gateway_client_cert.go:171 meta.SetStatusCondition merges same-type | One condition per type, merged not duplicated. |
| GW-80 | GatewayStatus.Conditions | CTRL | MET | gateway_controller.go:232,253 ObservedGeneration: freshGateway.Generation | observedGeneration set to metadata.generation at update time. |
| GW-81 | GatewayStatus.Conditions | CTRL | PARTIAL | gateway_controller.go:171 meta.SetStatusCondition (no observedGeneration regression guard) | meta.SetStatusCondition does not compare observedGeneration; controller relies on fresh Get + RetryOnConflict, not an explicit greater-than skip. |
| GW-82 | GatewayInfrastructure.Labels | N/A | NA | conformance_test.go:141 SupportGatewayInfrastructurePropagation Exempt | No created resources per Gateway (proxy is chart-owned); infra labels not propagated. |
| GW-83 | GatewayInfrastructure.Labels | N/A | NA | conformance_test.go:141 infra-propagation exempt | No per-Gateway Pods/resources to relabel. |
| GW-84 | GatewayInfrastructure.Annotations | N/A | NA | conformance_test.go:141 infra-propagation exempt | No per-Gateway resources to annotate. |
| GW-85 | GatewayInfrastructure.ParametersRef | CTRL | MET | resolver.go:91-101 parametersRef group/kind checks; gateway_controller.go:172-181 setConfigErrorStatus -> InvalidParameters | Missing/unsupported/malformed parametersRef -> Accepted=False/InvalidParameters. |
| GW-86 | GatewayReasonAddressNotAssigned | CTRL | GAP | gateway_controller.go no AddressNotAssigned emission | Reason never used (see GW-75); message guidance N/A because reason path is absent. |
| GW-87 | GatewayReasonAddressNotUsable | CTRL | GAP | gateway_controller.go no AddressNotUsable emission | Requested addresses never validated, so AddressNotUsable + prescriptive message never produced. |
| GW-88 | GatewayReasonInvalidClientCertificateRef | CTRL | MET | gateway_client_cert.go:218-221 reason only for in-ns or grant-allowed refs; denial uses RefNotPermitted | InvalidClientCertificateRef used only for allowed refs; cross-ns denial routed to RefNotPermitted. |
| GW-89 | ListenerStatus.SupportedKinds | CTRL | MET | gateway_controller.go:271,327 SupportedKinds = FilterSupportedKinds result | SupportedKinds reports the kinds the controller actually supports per listener. |
| GW-90 | ListenerStatus.SupportedKinds | CTRL | MET | gateway_controller.go:285-287 empties supportedKinds when no valid kind; kind.go:130-136 filters out unsupported | Unsupported specified kinds excluded from SupportedKinds. |
| GW-91 | ListenerStatus.SupportedKinds | CTRL | MET | gateway_controller.go:991-1010 InvalidRouteKinds reason | Unsupported specified kinds -> ResolvedRefs=False/InvalidRouteKinds. |
| GW-92 | ListenerStatus.SupportedKinds | CTRL | MET | kind.go:130-137 keeps valid kinds while flagging invalid | Mixed valid+invalid: valid kinds still listed in SupportedKinds. |
| GW-93 | ListenerStatus.AttachedRoutes | CTRL | MET | gateway_controller.go:207,328 countAttachedRoutes set per listener regardless of Accepted | AttachedRoutes counted for every listener. |
| GW-94 | ListenerStatus.AttachedRoutes | CTRL | MET | gateway_controller.go:437-445,474-481 only Accepted bindings counted | Only routes with Accepted binding increment the count. |
| GW-95 | ListenerStatus.Conditions | CTRL | MET | gateway_controller.go:198-201,333-335 RetryOnConflict fresh Get + Status().Update | Read-modify-write on listener conditions. |
| GW-96 | ListenerStatus.Conditions | CTRL | PARTIAL | gateway_controller.go:333 freshGateway.Status.Listeners = listenerStatuses (whole-slice replace) | Listener statuses are fully rebuilt+replaced each reconcile; foreign per-listener conditions not preserved (controller owns all listener conditions it writes). |
| GW-98 | ListenerStatus.Conditions | CTRL | PARTIAL | gateway_controller.go:314,333 conditions slice rebuilt then assigned | Same-type merge holds within the controller's own conditions, but the listener slice is reassigned rather than merged across reconcilers. |
| GW-99 | ListenerStatus.Conditions | CTRL | MET | gateway_controller.go:292,253 ObservedGeneration: freshGateway.Generation on listener conditions | observedGeneration set to metadata.generation. |
| GW-100 | ListenerStatus.Conditions | CTRL | PARTIAL | gateway_controller.go:333 whole-slice replace, no observedGeneration regression guard | No explicit greater-than check before updating a listener condition; relies on fresh Get + RetryOnConflict. |
| GW-101 | ListenerConditionOverlappingTLSConfig | N/A | NA | edge terminates TLS; conformance_test.go:234 HTTPRouteHTTPSListener skipped | Overlapping-TLS-hostname detection N/A; no in-cluster TLS listeners. |
| GW-102 | ListenerConditionOverlappingTLSConfig | N/A | NA | edge terminates TLS (no in-cluster certs) | Overlapping-certificate detection (MAY) N/A. |
| GW-103 | ListenerConditionOverlappingTLSConfig | N/A | NA | edge terminates TLS; no OverlappingTLSConfig condition emitted | Condition not applicable to a tunnel that does no in-cluster TLS. |
| GW-104 | ListenerConditionOverlappingTLSConfig | N/A | NA | edge terminates TLS | OverlappingCertificates reason N/A. |
| GW-105 | ListenerConditionOverlappingTLSConfig | N/A | NA | edge terminates TLS | Negative-polarity condition never set since no in-cluster TLS overlap detection. |
