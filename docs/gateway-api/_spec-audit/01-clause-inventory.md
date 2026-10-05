# Spec clause inventory (Phase 1)

Raw extraction of RFC-2119 keyword occurrences from the godoc of `sigs.k8s.io/gateway-api v1.6.2`, read from `vendor/sigs.k8s.io/gateway-api/apis/` while that version was vendored. This is the field-level normative source. Cross-cutting GEP/concept requirements (routing precedence narrative, policy attachment, status state machine) are collected separately in Phase 2.

Provenance: the extraction was performed at v1.5.1 and refreshed for the v1.5.1 → v1.6.0 tag diff (see `00-compliance-matrix.md`, "Baseline refreshes since v1.5.1"). In the v1.6.2 sync every `file:line` was re-pinned to the vendored v1.6.2 text by searching for the row's requirement wording, and the clauses the earlier passes left out were added (GW-108..GW-115, GC-24, GC-25, HR-73..HR-76, GR-54, POL-19).

Coverage check, v1.6.2: every godoc line carrying an uppercase keyword in the 18 vendored `apis/v1`, `apis/v1beta1` and `apis/v1alpha2` files that carry one sits within a row's location (one line before to eight lines after the cited line), except two restatements of GR-30 on `GRPCMethodMatch.Service` and `GRPCMethodMatch.Method`.

ID gaps (GW-03, HR-29, SH-01, LS-16 and the rest): IDs were assigned to keyword occurrences during the v1.5.1 extraction, and occurrences then folded into another row (a restatement, a sentence fragment, or a duplicate across served versions) kept no row. For uppercase keywords the coverage check above shows that no gap hides a clause. Lowercase occurrences were rowed only where they state a requirement the controller could meet or miss.

Schema: `ID | Field/Type | file:line | Keyword | Requirement` (paths relative to `apis/v1/` unless prefixed with another API version).

Caveat — RFC 8174: only ALL-CAPS forms (MUST, MUST NOT, SHOULD, SHOULD NOT, MAY, REQUIRED, RECOMMENDED, OPTIONAL) are normative. Rows whose Keyword column is lowercase (`must`, `should`, `may`, marked `(lc)`) are descriptive prose or schema constraints already enforced by CRD CEL/kubebuilder markers — kept here for completeness, classified as non-normative / CRD-enforced in Phase 3. The `UNLESS` rows are sentence fragments captured for context.

ID prefixes: GW=Gateway, HR=HTTPRoute, GR=GRPCRoute, SH=shared, GC=GatewayClass, RG=ReferenceGrant, OR=object_reference, BTLS=BackendTLSPolicy, POL=policy, LS=ListenerSet, OTHER=TLSRoute/TCPRoute/UDPRoute + v1alpha2/v1beta1 (exempt/secondary).

## Gateway (gateway_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| GW-01 | GatewaySpec.Listeners | gateway_types.go:76 | MUST | At least one Listener MUST be specified. |
| GW-02 | GatewaySpec.Listeners | gateway_types.go:80 | MUST | Each Listener in a set of Listeners MUST be distinct, in that a traffic flow MUST be able to be assigned to exactly one listener. |
| GW-04 | GatewaySpec.Listeners | gateway_types.go:83 | MAY | Implementations MAY merge configuration from multiple Gateways onto a single data plane, and these rules also apply in that case. |
| GW-05 | GatewaySpec.Listeners | gateway_types.go:87 | MUST | Each listener in a set MUST have a unique combination of Port, Protocol, and, if supported by the protocol, Hostname. |
| GW-06 | GatewaySpec.Listeners | gateway_types.go:90 | MUST | Some combinations of port, protocol, and TLS settings are considered Core support and MUST be supported by implementations based on the objects they support. |
| GW-07 | GatewaySpec.Listeners | gateway_types.go:113 | MUST | When multiple listeners have the same value for the Protocol field, then each of the Listeners with matching Protocol values MUST have different values for other fields. |
| GW-10 | GatewaySpec.Listeners | gateway_types.go:122 | MUST | The set of listeners that all share a protocol value MUST have different values for at least one of these fields to be distinct. |
| GW-11 | GatewaySpec.Listeners | gateway_types.go:136 | MUST NOT | All the Listeners that share a port with the TCP Listener are not distinct and so MUST NOT be accepted. |
| GW-12 | GatewaySpec.Listeners | gateway_types.go:139 | SHOULD NOT | If an implementation does not support TCP Protocol Listeners, the TCP Listeners SHOULD NOT be accepted. |
| GW-13 | GatewaySpec.Listeners | gateway_types.go:148 | MUST | When the Listeners are distinct based only on Hostname, inbound request hostnames MUST match from the most specific to least specific Hostname values to choose the correct Listener and its associated set of Routes. |
| GW-14 | GatewaySpec.Listeners | gateway_types.go:152 | MUST | Exact matches MUST be processed before wildcard matches, and wildcard matches MUST be processed before fallback (empty Hostname value) matches. |
| GW-16 | GatewaySpec.Listeners | gateway_types.go:170 | MUST | If a set of Listeners contains Listeners that are not distinct, then those Listeners are Conflicted, and the implementation MUST set the "Conflicted" condition in the Listener Status to "True". |
| GW-17 | GatewaySpec.Listeners | gateway_types.go:177 | MAY | Implementations MAY choose to accept a Gateway with some Conflicted Listeners only if they only accept the partial Listener set that contains no Conflicted Listeners. |
| GW-19 | GatewaySpec.Listeners | gateway_types.go:184 | MUST NOT | The implementation MUST NOT pick one conflicting Listener as the winner. ALL indistinct Listeners must not be accepted for processing. |
| GW-20 | GatewaySpec.Listeners | gateway_types.go:186 | MUST | At least one distinct Listener MUST be present, or else the Gateway effectively contains no Listeners, and must be rejected from processing as a whole. |
| GW-21 | GatewaySpec.Listeners | gateway_types.go:189 | MUST | The implementation MUST set a "ListenersNotValid" condition on the Gateway Status when the Gateway contains Conflicted Listeners whether or not they accept the Gateway. |
| GW-22 | GatewaySpec.Listeners | gateway_types.go:191 | SHOULD | That Condition SHOULD clearly indicate in the Message which Listeners are conflicted, and which are Accepted. |
| GW-23 | GatewaySpec.Listeners | gateway_types.go:193 | SHOULD | Additionally, the Listener status for those listeners SHOULD indicate which Listeners are conflicted and not Accepted. |
| GW-24 | GatewaySpec.Listeners | gateway_types.go:198 | SHOULD | For all distinct Listeners, requests SHOULD match at most one Listener. |
| GW-25 | GatewaySpec.Listeners | gateway_types.go:199 | SHOULD | If Listeners are defined for "foo.example.com" and "*.example.com", a request to "foo.example.com" SHOULD only be routed using routes attached to the "foo.example.com" Listener. |
| GW-26 | GatewaySpec.Listeners | gateway_types.go:210 | MUST | Implementations that do not support Listener Isolation MUST clearly document this. |
| GW-27 | GatewaySpec.Listeners | gateway_types.go:210 | MUST NOT | Implementations that do not support Listener Isolation MUST NOT claim support for the `GatewayHTTPListenerIsolation` feature. |
| GW-28 | GatewaySpec.Listeners | gateway_types.go:214 | SHOULD | Implementations that do support Listener Isolation SHOULD claim support for the Extended `GatewayHTTPListenerIsolation` feature and pass the associated conformance tests. |
| GW-29 | GatewaySpec.Addresses | gateway_types.go:235 | MAY | Implementations MAY merge separate Gateways onto a single set of Addresses if all Listeners across all Gateways are compatible. |
| GW-31 | GatewaySpec.Addresses | gateway_types.go:256 | MUST | If a value is set in the spec and the requested address is invalid or unavailable, the implementation MUST indicate this in an associated entry in GatewayStatus.Conditions. |
| GW-32 | GatewaySpec.Addresses | gateway_types.go:266 | MAY | If no Addresses are specified, the implementation MAY schedule the Gateway in an implementation-specific manner, assigning an appropriate set of Addresses. |
| GW-33 | GatewaySpec.Addresses | gateway_types.go:270 | MUST | The implementation MUST bind all Listeners to every GatewayAddress that it assigns to the Gateway and add a corresponding entry in GatewayStatus.Addresses. |
| GW-34 | Listener.Name | gateway_types.go:365 | MUST | Name is the name of the Listener. This name MUST be unique within a Gateway. |
| GW-35 | Listener.Hostname | gateway_types.go:377 | MUST | Implementations MUST apply Hostname matching appropriately for each of the following protocols. |
| GW-36 | Listener.Hostname | gateway_types.go:380 | MUST | TLS: The Listener Hostname MUST match the SNI. |
| GW-37 | Listener.Hostname | gateway_types.go:381 | MUST | HTTP: The Listener Hostname MUST match the Host header of the request. |
| GW-38 | Listener.Hostname | gateway_types.go:382 | SHOULD | HTTPS: The Listener Hostname SHOULD match both the SNI and Host header. |
| GW-39 | Listener.Hostname | gateway_types.go:386 | MUST | Section 11.1 of RFC-6066 emphasizes that server implementations that rely on SNI hostname matching MUST also verify hostnames within the application protocol. |
| GW-40 | Listener.Hostname | gateway_types.go:395 | SHOULD | To detect misdirected requests, Gateways SHOULD match the authority of the requests with all the SNI hostname(s) configured across all the Gateway Listeners on the same port and protocol. |
| GW-41 | Listener.Hostname | gateway_types.go:399 | SHOULD | If another Listener has an exact match or more specific wildcard entry, the Gateway SHOULD return a 421. |
| GW-42 | Listener.Hostname | gateway_types.go:403 | SHOULD | If another Listener does match the Host, the Gateway SHOULD return a 421. |
| GW-43 | Listener.Hostname | gateway_types.go:405 | MUST | If no other Listener matches the Host, the Gateway MUST return a 404. |
| GW-44 | Listener.Hostname | gateway_types.go:409 | MUST | When both listener and route specify hostnames, there MUST be an intersection between the values for a Route to be accepted. |
| GW-45 | Listener.TLS | gateway_types.go:447 | MUST | The GatewayClass MUST use the longest matching SNI out of all available certificates for any TLS handshake. |
| GW-46 | Listener.AllowedRoutes | gateway_types.go:455 | MAY | AllowedRoutes defines the types of routes that MAY be attached to a Listener and the trusted namespaces where those Route resources MAY be present. |
| GW-48 | Listener.AllowedRoutes | gateway_types.go:460 | MUST | Matching precedence MUST be determined in order of the following criteria. |
| GW-49 | ProtocolType | gateway_types.go:485 | MUST | If an implementation does not support a specified protocol, it MUST set the "Accepted" condition to False for the affected Listener with a reason of "UnsupportedProtocol". |
| GW-50 | HTTPProtocolType | gateway_types.go:514 | MAY | Accepts cleartext HTTP/1.1 sessions over TCP. Implementations MAY also support HTTP/2 over cleartext. |
| GW-51 | HTTPProtocolType | gateway_types.go:515 | MUST | If implementations support HTTP/2 over cleartext on "HTTP" listeners, that MUST be clearly documented by the implementation. |
| GW-52 | GatewayBackendTLS.ClientCertificateRef | gateway_types.go:543 | MUST | The `ResolvedRefs` condition on the Gateway MUST be set to False with the Reason `InvalidClientCertificateRef` and the Message of the Condition MUST indicate why the reference is invalid. |
| GW-54 | GatewayBackendTLS.ClientCertificateRef | gateway_types.go:547 | UNLESS | It refers to a resource in another namespace UNLESS there is a ReferenceGrant in the target namespace that allows the certificate to be attached. |
| GW-55 | GatewayBackendTLS.ClientCertificateRef | gateway_types.go:549 | MUST | If a ReferenceGrant does not allow this reference, the `ResolvedRefs` condition on the Gateway MUST be set to False with the Reason `RefNotPermitted`. |
| GW-56 | GatewayBackendTLS.ClientCertificateRef | gateway_types.go:552 | MAY | Implementations MAY choose to perform further validation of the certificate content. |
| GW-57 | GatewayBackendTLS.ClientCertificateRef | gateway_types.go:553 | MUST | In such cases, an implementation-specific Reason and Message MUST be set. |
| GW-58 | ListenerTLSConfig.CertificateRefs | gateway_types.go:591 | MAY | Implementations MAY choose to support attaching multiple certificates to a Listener, but this behavior is implementation-specific. |
| GW-59 | ListenerTLSConfig.CertificateRefs | gateway_types.go:594 | UNLESS | References to a resource in different namespace are invalid UNLESS there is a ReferenceGrant in the target namespace that allows the certificate to be attached. |
| GW-60 | ListenerTLSConfig.CertificateRefs | gateway_types.go:596 | MUST | If a ReferenceGrant does not allow this reference, the "ResolvedRefs" condition MUST be set to False for this listener with the "RefNotPermitted" reason. |
| GW-61 | ListenerTLSConfig.Options | gateway_types.go:619 | MAY | A set of common keys MAY be defined by the API in the future. |
| GW-62 | ListenerTLSConfig.Options | gateway_types.go:619 | MUST | To avoid any ambiguity, implementation-specific definitions MUST use domain-prefixed names. |
| GW-63 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:761 | MAY | Implementations MAY choose to perform further validation of the certificate content. |
| GW-64 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:763 | MUST | In such cases, an implementation-specific Reason and Message MUST be set. |
| GW-65 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:765 | MUST | The implementation MUST ensure that the `ResolvedRefs` condition is set to `status: False` on all targeted listeners. |
| GW-66 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:767 | MUST | The condition MUST include a Reason and Message that indicate the cause of the error. |
| GW-67 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:768 | MUST | If ALL CACertificateRefs are invalid, the implementation MUST also ensure the `Accepted` condition on the listener is set to `status: False`, with the Reason `NoValidCACertificate`. |
| GW-68 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:772 | MAY | Implementations MAY choose to support attaching multiple CA certificates to a listener, but this behavior is implementation-specific. |
| GW-69 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:755 | UNLESS | It refers to a resource in another namespace UNLESS there is a ReferenceGrant in the target namespace that allows the CA certificate to be attached. |
| GW-70 | FrontendTLSValidation.CACertificateRefs | gateway_types.go:757 | MUST | If a ReferenceGrant does not allow this reference, the `ResolvedRefs` on all matching HTTPS listeners condition MUST be set with the Reason `RefNotPermitted`. |
| GW-71 | AllowValidOnly | gateway_types.go:815 | MUST | AllowValidOnly indicates that a client certificate is required during the TLS handshake and MUST pass validation. |
| GW-72 | AllowedRoutes.Kinds | gateway_types.go:843 | MUST | A RouteGroupKind MUST correspond to kinds of Routes that are compatible with the application protocol specified in the Listener's Protocol field. |
| GW-73 | AllowedRoutes.Kinds | gateway_types.go:845 | MUST | If an implementation does not support or recognize this resource type, it MUST set the "ResolvedRefs" condition to False for this Listener with the "InvalidRouteKinds" reason. |
| GW-74 | GatewaySpecAddress.Value | gateway_types.go:924 | SHOULD | When a value is unspecified, an implementation SHOULD automatically assign an address matching the requested type if possible. |
| GW-75 | GatewaySpecAddress.Value | gateway_types.go:927 | MUST | If an implementation does not support an empty value, they MUST set the "Programmed" condition in status to False with a reason of "AddressNotAssigned". |
| GW-76 | GatewayStatus.Conditions | gateway_types.go:998 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| GW-77 | GatewayStatus.Conditions | gateway_types.go:1002 | MUST NOT | Implementations MUST NOT remove or reorder Conditions that they are not directly responsible for. |
| GW-78 | GatewayStatus.Conditions | gateway_types.go:1003 | MUST NOT | If an implementation sees a Condition with type `special.io/SomeField`, it MUST NOT remove, change or update that Condition. |
| GW-79 | GatewayStatus.Conditions | gateway_types.go:1006 | MUST | Implementations MUST always merge changes into Conditions of the same Type, rather than creating more than one Condition of the same Type. |
| GW-80 | GatewayStatus.Conditions | gateway_types.go:1008 | MUST | Implementations MUST always update the `observedGeneration` field of the Condition to the `metadata.generation` of the Gateway at the time of update creation. |
| GW-81 | GatewayStatus.Conditions | gateway_types.go:1010 | MUST NOT | If the `observedGeneration` of a Condition is greater than the value the implementation knows about, then it MUST NOT perform the update on that Condition. |
| GW-82 | GatewayInfrastructure.Labels | gateway_types.go:1048 | SHOULD | Labels that SHOULD be applied to any resources created in response to this Gateway. |
| GW-83 | GatewayInfrastructure.Labels | gateway_types.go:1055 | SHOULD | If an implementation maps these labels to Pods, or any other resource that would need to be recreated when labels change, it SHOULD clearly warn about this behavior in documentation. |
| GW-84 | GatewayInfrastructure.Annotations | gateway_types.go:1066 | SHOULD | Annotations that SHOULD be applied to any resources created in response to this Gateway. |
| GW-85 | GatewayInfrastructure.ParametersRef | gateway_types.go:1091 | SHOULD | If the referent cannot be found, refers to an unsupported kind, or when the data within that resource is malformed, the Gateway SHOULD be rejected with the "Accepted" status condition set to "False" and an "InvalidParameters" reason. |
| GW-86 | GatewayReasonAddressNotAssigned | gateway_types.go:1186 | SHOULD | When this reason is used the implementation SHOULD provide a clear message explaining the underlying problem. |
| GW-87 | GatewayReasonAddressNotUsable | gateway_types.go:1201 | SHOULD | When this reason is used the implementation SHOULD provide prescriptive information on which address is causing the problem and how to resolve it in the condition message. |
| GW-88 | GatewayReasonInvalidClientCertificateRef | gateway_types.go:1326 | MUST (lc) | This reason must be used only when the reference is allowed, either by referencing an object in the same namespace as the Gateway, or when a cross-namespace reference has been explicitly allowed by a ReferenceGrant. |
| GW-89 | ListenerStatus.SupportedKinds | gateway_types.go:1374 | MUST | SupportedKinds MUST represent the kinds supported by an implementation for that Listener configuration. |
| GW-90 | ListenerStatus.SupportedKinds | gateway_types.go:1377 | MUST NOT | If kinds are specified in Spec that are not supported, they MUST NOT appear in this list. |
| GW-91 | ListenerStatus.SupportedKinds | gateway_types.go:1377 | MUST | If kinds are specified in Spec that are not supported, an implementation MUST set the "ResolvedRefs" condition to "False" with the "InvalidRouteKinds" reason. |
| GW-92 | ListenerStatus.SupportedKinds | gateway_types.go:1379 | MUST | If both valid and invalid Route kinds are specified, the implementation MUST reference the valid Route kinds that have been specified. |
| GW-93 | ListenerStatus.AttachedRoutes | gateway_types.go:1399 | MUST | The AttachedRoutes field count MUST be set for Listeners, even if the Accepted condition of an individual Listener is set to "False". |
| GW-94 | ListenerStatus.AttachedRoutes | gateway_types.go:1403 | MUST NOT | Routes with any other value for the Accepted condition MUST NOT be included in this count. |
| GW-95 | ListenerStatus.Conditions | gateway_types.go:1423 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| GW-96 | ListenerStatus.Conditions | gateway_types.go:1427 | MUST NOT | Implementations MUST NOT remove or reorder Conditions that they are not directly responsible for. |
| GW-98 | ListenerStatus.Conditions | gateway_types.go:1431 | MUST | Implementations MUST always merge changes into Conditions of the same Type. |
| GW-99 | ListenerStatus.Conditions | gateway_types.go:1433 | MUST | Implementations MUST always update the `observedGeneration` field of the Condition to the `metadata.generation` of the Gateway at the time of update creation. |
| GW-100 | ListenerStatus.Conditions | gateway_types.go:1435 | MUST NOT | If the `observedGeneration` of a Condition is greater than the value the implementation knows about, then it MUST NOT perform the update on that Condition. |
| GW-101 | ListenerConditionOverlappingTLSConfig | gateway_types.go:1686 | MUST | Controllers MUST detect the presence of overlapping hostnames and MAY detect the presence of overlapping certificates. |
| GW-102 | ListenerConditionOverlappingTLSConfig | gateway_types.go:1686 | MAY | Controllers MUST detect the presence of overlapping hostnames and MAY detect the presence of overlapping certificates. |
| GW-103 | ListenerConditionOverlappingTLSConfig | gateway_types.go:1689 | MUST | This condition MUST be set on all Listeners with overlapping TLS config. |
| GW-104 | ListenerConditionOverlappingTLSConfig | gateway_types.go:1705 | SHOULD | If a controller supports checking for both possible reasons and finds that both are true, it SHOULD set the "OverlappingCertificates" Reason. |
| GW-105 | ListenerConditionOverlappingTLSConfig | gateway_types.go:1708 | MUST NOT | This is a negative polarity condition and MUST NOT be set when it is False. |
| GW-106 | GatewaySpec.Listeners | gateway_types.go:203 (v1.6.0) | MUST | If traffic to a Gateway does not match any Listener's hostname (or if the Listener does not specify a hostname and the request does not match any attached Route), the request MUST be rejected. The specific mechanism for rejection depends on the protocol: HTTP returns a 404 status code, while gRPC returns an Unimplemented status code. (Added in v1.6.0 by kubernetes-sigs/gateway-api#4408; appended out of line order to keep pre-existing IDs stable.) |
| GW-107 | GatewayConditionAccepted | gateway_types.go:1247 | may/should (lc) | Controllers may raise this condition with other reasons, but should prefer to use the reasons listed above to improve interoperability. (Listed False reasons include "InvalidParameters" — "the parametersRef field was invalid", gateway_types.go:1273-1276. Captured for the v3.5 tunnel-arbitration refusal, which reuses that reason for a referent that resolves but names a tunnel held elsewhere; appended out of line order to keep pre-existing IDs stable.) |
| GW-108 | GatewayNameLabelKey | well_known_labels.go:24 | must (lc) | All generated resources must include a label set to the name of the Gateway resource. (GEP-1762, kubernetes-sigs/gateway-api#4705. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-109 | GatewayClassNameLabelKey | well_known_labels.go:27 | should (lc) | All generated resources should include a label set to the name of the GatewayClass resource. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-110 | RouteNamespaces.Selector | gateway_types.go:891 | must (lc) | Selector must be specified when From is set to "Selector". In that case, only Routes in Namespaces matching this Selector will be selected by this Gateway. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-111 | ListenerNamespaces.Selector | gateway_types.go:354 | must (lc) | Selector must be specified when From is set to "Selector". In that case, only ListenerSets in Namespaces matching this Selector will be selected by this Gateway. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-112 | ListenerReasonInvalidCertificateRef | gateway_types.go:1597 | must (lc) | This reason must be used only when the reference is allowed, either by referencing an object in the same namespace as the Gateway, or when a cross-namespace reference has been explicitly allowed by a ReferenceGrant. If the reference is not allowed, the reason RefNotPermitted must be used instead. (Listener twin of GW-88. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-113 | GatewaySpec.Listeners | gateway_types.go:157 | must (lc) | Additionally, if there are multiple wildcard entries, more specific wildcard entries must be processed before less specific wildcard entries. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-114 | Gateway | gateway_types.go:35 | SHOULD | A Gateway name SHOULD be compliant with RFC 1035, consisting of a maximum of 63 lower case alphanumeric characters or hyphens ('-'). (New in the v1.6.0 godoc, missed by the v1.6.0 refresh. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GW-115 | Gateway | gateway_types.go:36 | MUST | A Gateway name MUST start and end with an alphanumeric character. (New in the v1.6.0 godoc, missed by the v1.6.0 refresh. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |

## HTTPRoute (httproute_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| HR-01 | HTTPRouteSpec.Hostnames | httproute_types.go:63 | MUST | Implementations MUST ignore any port value specified in the HTTP Host header while performing a match. |
| HR-02 | HTTPRouteSpec.Hostnames | httproute_types.go:66 | MUST | Absent of any applicable header modification configuration, implementations MUST forward this header unmodified to the backend. |
| HR-03 | HTTPRouteSpec.Hostnames | httproute_types.go:93 | MUST | If both the Listener and HTTPRoute have specified hostnames, any HTTPRoute hostnames that do not match the Listener hostname MUST be ignored. |
| HR-04 | HTTPRouteRule.Name | httproute_types.go:148 | MUST | Name is the name of the route rule. This name MUST be unique within a Route if it is set. |
| HR-05 | HTTPRouteRule.Matches | httproute_types.go:184 | MUST | Proxy or Load Balancer routing configuration generated from HTTPRoutes MUST prioritize matches based on the following criteria, continuing on ties. |
| HR-06 | HTTPRouteRule.Matches | httproute_types.go:197 | MUST | If ties still exist across multiple Routes, matching precedence MUST be determined in order of the following criteria, continuing on ties. |
| HR-07 | HTTPRouteRule.Matches | httproute_types.go:204 | MUST | If ties still exist within an HTTPRoute, matching precedence MUST be granted to the FIRST matching rule (in list order) with a match meeting the above criteria. |
| HR-08 | HTTPRouteRule.Matches | httproute_types.go:208 | MUST | When no rules matching a request have been successfully attached to the parent a request is coming from, a HTTP 404 status code MUST be returned. |
| HR-09 | HTTPRouteRule.Filters | httproute_types.go:220 | SHOULD | Wherever possible, implementations SHOULD implement filters in the order they are specified. |
| HR-10 | HTTPRouteRule.Filters | httproute_types.go:223 | MAY | Implementations MAY choose to implement this ordering strictly, rejecting any combination or order of filters that cannot be supported. |
| HR-11 | HTTPRouteRule.Filters | httproute_types.go:224 | MUST | If implementations choose a strict interpretation of filter ordering, they MUST clearly document that behavior. |
| HR-12 | HTTPRouteRule.Filters | httproute_types.go:228 | SHOULD | To reject an invalid combination or order of filters, implementations SHOULD consider the Route Rules with this configuration invalid. |
| HR-13 | HTTPRouteRule.Filters | httproute_types.go:230 | MUST | If only a portion of Route Rules are invalid, implementations MUST set the "PartiallyInvalid" condition for the Route. |
| HR-14 | HTTPRouteRule.Filters | httproute_types.go:236 | MUST | ALL core filters MUST be supported by all implementations. |
| HR-15 | HTTPRouteRule.BackendRefs | httproute_types.go:271 | MUST | If all entries in BackendRefs are invalid, and there are also no filters specified in this route rule, all traffic which matches this rule MUST receive a 500 status code. |
| HR-16 | HTTPRouteRule.BackendRefs | httproute_types.go:278 | MUST | When a HTTPBackendRef is invalid, 500 status codes MUST be returned for requests that would have otherwise been routed to an invalid backend. |
| HR-17 | HTTPRouteRule.BackendRefs | httproute_types.go:279 | MUST | If multiple backends are specified, and some are invalid, the proportion of requests that would otherwise have been routed to an invalid backend MUST receive a 500 status code. |
| HR-18 | HTTPRouteRule.BackendRefs | httproute_types.go:288 | SHOULD | When a HTTPBackendRef refers to a Service that has no ready endpoints, implementations SHOULD return a 503 for requests to that backend instead. |
| HR-19 | HTTPRouteRule.BackendRefs | httproute_types.go:290 | MUST | If an implementation chooses to do this, all of the above rules for 500 responses MUST also apply for responses that return a 503. |
| HR-20 | HTTPRouteTimeouts.Request | httproute_types.go:337 | MUST | If the gateway has not been able to respond before this deadline is met, the gateway MUST return a timeout error. |
| HR-21 | HTTPRouteTimeouts.Request | httproute_types.go:344 | SHOULD | Setting a timeout to the zero duration (e.g. "0s") SHOULD disable the timeout completely. |
| HR-22 | HTTPRouteTimeouts.Request | httproute_types.go:345 | MUST | Implementations that cannot completely disable the timeout MUST instead interpret the zero duration as the longest possible value to which the timeout can be set. |
| HR-23 | HTTPRouteTimeouts.Request | httproute_types.go:350 | MAY | An implementation MAY choose to start the timeout after the entire request stream has been received instead of immediately after the transaction is initiated by the client. |
| HR-24 | HTTPRouteTimeouts.BackendRequest | httproute_types.go:366 | SHOULD | Setting a timeout to the zero duration (e.g. "0s") SHOULD disable the timeout completely. |
| HR-25 | HTTPRouteTimeouts.BackendRequest | httproute_types.go:367 | MUST | Implementations that cannot completely disable the timeout MUST instead interpret the zero duration as the longest possible value to which the timeout can be set. |
| HR-26 | HTTPRouteRetry | httproute_types.go:388 | SHOULD | Implementations SHOULD retry on connection errors (disconnect, reset, timeout, TCP failure) if a retry stanza is configured. |
| HR-27 | HTTPRouteRetry.Attempts | httproute_types.go:403 | MUST | If the maximum number of retries has been attempted without a successful response from the backend, the Gateway MUST return an error. |
| HR-28 | HTTPRouteRetry.Backoff | httproute_types.go:423 | MAY | An implementation MAY use an exponential or alternative backoff strategy for subsequent retry attempts, as long as unsuccessful backend requests are not retried before the configured minimum duration. |
| HR-31 | HTTPRouteRetry.Backoff | httproute_types.go:429 | MUST | If a Request timeout is configured on the route, the entire duration of the initial request and any retry attempts MUST not exceed the Request timeout duration. |
| HR-32 | HTTPRouteRetry.Backoff | httproute_types.go:431 | SHOULD | If any retry attempts are still in progress when the Request timeout duration has been reached, these SHOULD be canceled if possible. |
| HR-33 | HTTPRouteRetry.Backoff | httproute_types.go:432 | MUST | When the Request timeout duration has been reached, the Gateway MUST immediately return a timeout error. |
| HR-34 | HTTPRouteRetry.Backoff | httproute_types.go:436 | SHOULD | If a BackendRequest timeout is configured, any retry attempts which reach the configured BackendRequest timeout duration without a response SHOULD be canceled if possible. |
| HR-35 | HTTPRouteRetry.Backoff | httproute_types.go:442 | MAY | If a BackendRequest timeout is not configured, retry attempts MAY time out after an implementation default duration, or MAY remain pending until a configured Request timeout or implementation default duration is reached. |
| HR-37 | HTTPRouteRetryStatusCode | httproute_types.go:459 | MUST | Implementations MUST support the following status codes as retriable. |
| HR-38 | HTTPRouteRetryStatusCode | httproute_types.go:466 | MAY | Implementations MAY support specifying additional discrete values in the 500-599 range. |
| HR-39 | HTTPRouteRetryStatusCode | httproute_types.go:469 | MAY | Implementations MAY support specifying discrete values in the 400-499 range, which are often inadvisable to retry. |
| HR-40 | HTTPHeaderMatch.Name | httproute_types.go:614 | MUST | Name is the name of the HTTP Header to be matched. Name matching MUST be case-insensitive. |
| HR-41 | HTTPHeaderMatch.Name | httproute_types.go:617 | MUST | If multiple entries specify equivalent header names, only the first entry with an equivalent name MUST be considered for a match. |
| HR-42 | HTTPHeaderMatch.Name | httproute_types.go:618 | MUST | Subsequent entries with an equivalent header name MUST be ignored. |
| HR-43 | HTTPQueryParamMatch.Name | httproute_types.go:689 | MUST | If multiple entries specify equivalent query param names, only the first entry with an equivalent name MUST be considered for a match. |
| HR-44 | HTTPQueryParamMatch.Name | httproute_types.go:690 | MUST | Subsequent entries with an equivalent query param name MUST be ignored. |
| HR-45 | HTTPQueryParamMatch.Name | httproute_types.go:700 | SHOULD NOT | Users SHOULD NOT route traffic based on repeated query params to guard themselves against potential differences in the implementations. |
| HR-46 | HTTPRouteFilter.Type | httproute_types.go:847 | MUST NOT | If a reference to a custom filter type cannot be resolved, the filter MUST NOT be skipped. |
| HR-47 | HTTPRouteFilter.Type | httproute_types.go:848 | MUST | Instead, requests that would have been processed by that filter MUST receive a HTTP error response. |
| HR-48 | HTTPRouteFilter.ExternalAuth | httproute_types.go:919 | MUST | The external service MUST authenticate the request, and MAY authorize the request as well. |
| HR-50 | HTTPRouteFilter.ExternalAuth | httproute_types.go:922 | MUST | If there is any problem communicating with the external service, this filter MUST fail closed. |
| HR-51 | HTTPRouteFilter.ExtensionRef | httproute_types.go:933 | MUST NOT | ExtensionRef MUST NOT be used for core and extended filters. |
| HR-52 | HTTPRouteFilterRequestMirror | httproute_types.go:985 | MUST | The responses from this backend MUST be ignored by the Gateway. |
| HR-53 | HTTPRouteFilterExternalAuth | httproute_types.go:1002 | MUST | The external Auth server MUST perform Authentication and MAY perform Authorization on the matched request before the request is forwarded to the backend. |
| HR-58 | HTTPRequestRedirectFilter | httproute_types.go:1207 | MUST NOT | This filter MUST NOT be used on the same Route rule as a HTTPURLRewrite filter. |
| HR-59 | HTTPRequestRedirectFilter.Port | httproute_types.go:1250 | MUST | If no port is specified, the redirect port MUST be derived using the following rules. |
| HR-60 | HTTPRequestRedirectFilter.Port | httproute_types.go:1253 | MUST | If redirect scheme is not-empty, the redirect port MUST be the well-known port associated with the redirect scheme. |
| HR-61 | HTTPRequestRedirectFilter.Port | httproute_types.go:1255 | SHOULD | If the redirect scheme does not have a well-known port, the listener port of the Gateway SHOULD be used. |
| HR-62 | HTTPRequestRedirectFilter.Port | httproute_types.go:1257 | MUST | If redirect scheme is empty, the redirect port MUST be the Gateway Listener port. |
| HR-63 | HTTPRequestRedirectFilter.Port | httproute_types.go:1260 | SHOULD NOT | Implementations SHOULD NOT add the port number in the 'Location' header in the following cases. |
| HR-64 | HTTPURLRewriteFilter | httproute_types.go:1301 | MUST NOT | At most one of these filters may be used on a Route rule. This MUST NOT be used on the same Route rule as a HTTPRequestRedirect filter. |
| HR-65 | HTTPAuthConfig.Path | httproute_types.go:1734 | MUST | Even with the validation, implementations MUST sanitize this input before using it directly. |
| HR-67 | HTTPBackendRef | httproute_types.go:1813 | SHOULD | When the BackendRef points to a Kubernetes Service, implementations SHOULD honor the appProtocol field if it is set for the target Service Port. |
| HR-68 | HTTPBackendRef | httproute_types.go:1816 | SHOULD | Implementations supporting appProtocol SHOULD recognize the Kubernetes Standard Application Protocols defined in KEP-3726. |
| HR-69 | HTTPBackendRef | httproute_types.go:1819 | MAY | If a Service appProtocol isn't specified, an implementation MAY infer the backend protocol through its own means. |
| HR-71 | HTTPBackendRef | httproute_types.go:1823 | MUST | If a Route is not able to send traffic to the backend using the specified protocol then the backend is considered invalid. Implementations MUST set the "ResolvedRefs" condition to "False" with the "UnsupportedProtocol" reason. |
| HR-72 | HTTPBackendRef.BackendRef | httproute_types.go:1831 | MUST | A BackendRef can be invalid for various reasons. In all cases, the implementation MUST ensure the `ResolvedRefs` Condition on the Route is set to `status: False`, with a Reason and Message that indicate the cause of the error. |
| HR-73 | HTTPHeader.Name | httproute_types.go:1024 | MUST | Name is the name of the HTTP Header to be matched. Name matching MUST be case-insensitive. (HTTPHeader is the entry type of the RequestHeaderModifier / ResponseHeaderModifier `set` and `add` lists, for HTTPRoute and GRPCRoute. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| HR-74 | HTTPHeader.Name | httproute_types.go:1027 | MUST | If multiple entries specify equivalent header names, the first entry with an equivalent name MUST be considered for a match. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| HR-75 | HTTPHeader.Name | httproute_types.go:1029 | MUST | Subsequent entries with an equivalent header name MUST be ignored. Due to the case-insensitivity of header names, "foo" and "Foo" are considered equivalent. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| HR-76 | HTTPBackendRef.BackendRef | httproute_types.go:1854 | MUST (via HR-72) | A BackendRef is invalid when the BackendTLSPolicy object is installed in the cluster, a BackendTLSPolicy is present that refers to the Service, and the implementation is unable to meet the requirement; HR-72 then requires ResolvedRefs=False with a Reason and Message. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |

## GRPCRoute (grpcroute_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| GR-01 | GRPCRoute | grpcroute_types.go:37 | MUST | An implementation supporting GRPCRoute must conform to the indicated requirement, but an implementation not supporting this route type need not follow the requirement unless explicitly indicated. |
| GR-02 | GRPCRoute | grpcroute_types.go:42 | MUST | Implementations supporting GRPCRoute with the HTTPS ProtocolType MUST accept HTTP/2 connections without an initial upgrade from HTTP/1.1, i.e. via ALPN. |
| GR-03 | GRPCRoute | grpcroute_types.go:44 | MUST | If the implementation does not support this, then it MUST set the "Accepted" condition to "False" for the affected listener with a reason of "UnsupportedProtocol". |
| GR-04 | GRPCRoute | grpcroute_types.go:46 | MAY | Implementations MAY also accept HTTP/2 connections with an upgrade from HTTP/1. |
| GR-05 | GRPCRoute | grpcroute_types.go:49 | MUST | Implementations supporting GRPCRoute with the HTTP ProtocolType MUST support HTTP/2 over cleartext TCP (h2c) without an initial upgrade from HTTP/1.1, i.e. with prior knowledge. |
| GR-06 | GRPCRoute | grpcroute_types.go:53 | MUST | If the implementation does not support this, then it MUST set the "Accepted" condition to "False" for the affected listener with a reason of "UnsupportedProtocol". |
| GR-07 | GRPCRoute | grpcroute_types.go:56 | MAY | Implementations MAY also accept HTTP/2 connections with an upgrade from HTTP/1, i.e. without prior knowledge. |
| GR-08 | GRPCRouteSpec.Hostnames | grpcroute_types.go:95 | MUST | A hostname may be prefixed with a wildcard label (`*.`). The wildcard label MUST appear by itself as the first label. |
| GR-09 | GRPCRouteSpec.Hostnames | grpcroute_types.go:98 | MUST | If a hostname is specified by both the Listener and GRPCRoute, there MUST be at least one intersecting hostname for the GRPCRoute to be attached to the Listener. |
| GR-10 | GRPCRouteSpec.Hostnames | grpcroute_types.go:115 | MUST | If both the Listener and GRPCRoute have specified hostnames, any GRPCRoute hostnames that do not match the Listener hostname MUST be ignored. |
| GR-11 | GRPCRouteSpec.Hostnames | grpcroute_types.go:119 | MUST NOT | If a Listener specified `*.example.com`, and the GRPCRoute specified `test.example.com` and `test.example.net`, `test.example.net` MUST NOT be considered for a match. |
| GR-12 | GRPCRouteSpec.Hostnames | grpcroute_types.go:121 | MUST NOT | If both the Listener and GRPCRoute have specified hostnames, and none match with the criteria above, then the GRPCRoute MUST NOT be accepted by the implementation. |
| GR-13 | GRPCRouteSpec.Hostnames | grpcroute_types.go:123 | MUST | The implementation MUST raise an 'Accepted' Condition with a status of `False` in the corresponding RouteParentStatus. |
| GR-14 | GRPCRouteSpec.Hostnames | grpcroute_types.go:126 | MUST | If a Route (A) of type HTTPRoute or GRPCRoute is attached to a Listener that already has another Route (B) of the other type attached and the hostname intersection is non-empty, then the implementation MUST accept exactly one of these two routes (oldest by creation timestamp, then first alphabetically by namespace/name). v1.6.0 note: the site docs relaxed this to MAY (implementations "may enforce uniqueness" / "may reject Route A" — kubernetes-sigs/gateway-api#4598, `site-src/api-types/grpcroute.md` only), while the API godoc at the v1.6.0 tag still carries this MUST wording — an upstream doc inconsistency worth an upstream issue. Keyword kept as extracted from the vendored godoc. |
| GR-15 | GRPCRouteSpec.Hostnames | grpcroute_types.go:136 | MUST | The rejected Route MUST raise an 'Accepted' condition with a status of 'False' in the corresponding RouteParentStatus. v1.6.0 note: same #4598 site-docs MAY relaxation as GR-14 (rejection itself is now optional per the site docs; the condition wording applies when an implementation does reject). |
| GR-16 | GRPCRouteRule.Name | grpcroute_types.go:160 | MUST | Name is the name of the route rule. This name MUST be unique within a Route if it is set. |
| GR-17 | GRPCRouteRule.Matches | grpcroute_types.go:183 | MUST | For a request to match against this rule, it MUST satisfy EITHER of the two conditions. |
| GR-18 | GRPCRouteRule.Matches | grpcroute_types.go:192 | MUST | If no matches are specified, the implementation MUST match every gRPC request. |
| GR-19 | GRPCRouteRule.Matches | grpcroute_types.go:194 | MUST | Proxy or Load Balancer routing configuration generated from GRPCRoutes MUST prioritize rules based on the following criteria, continuing on ties. |
| GR-20 | GRPCRouteRule.Matches | grpcroute_types.go:196 | MUST | Merging MUST not be done between GRPCRoutes and HTTPRoutes. |
| GR-21 | GRPCRouteRule.Matches | grpcroute_types.go:197 | MUST | Precedence MUST be given to the rule with the largest number of: characters in a matching non-wildcard hostname; characters in a matching hostname; characters in a matching service; characters in a matching method; header matches. |
| GR-22 | GRPCRouteRule.Matches | grpcroute_types.go:205 | MUST | If ties still exist across multiple Routes, matching precedence MUST be determined by oldest Route by creation timestamp, then first alphabetically by namespace/name. |
| GR-23 | GRPCRouteRule.Matches | grpcroute_types.go:212 | MUST | If ties still exist within the Route that has been given precedence, matching precedence MUST be granted to the first matching rule meeting the above criteria. |
| GR-24 | GRPCRouteRule.Filters | grpcroute_types.go:229 | MUST | ALL core filters MUST be supported by all implementations that support GRPCRoute. |
| GR-25 | GRPCRouteRule.BackendRefs | grpcroute_types.go:259 | MUST | If all entries in BackendRefs are invalid, and there are also no filters specified, all traffic which matches this rule MUST receive an UNAVAILABLE status. |
| GR-26 | GRPCRouteRule.BackendRefs | grpcroute_types.go:266 | MUST | When a GRPCBackendRef is invalid, UNAVAILABLE statuses MUST be returned for requests that would have otherwise been routed to an invalid backend. |
| GR-27 | GRPCRouteRule.BackendRefs | grpcroute_types.go:267 | MUST | If multiple backends are specified, and some are invalid, the proportion of requests that would otherwise have been routed to an invalid backend MUST receive an UNAVAILABLE status. |
| GR-29 | GRPCRouteMatch.Headers | grpcroute_types.go:321 | MUST | Multiple match values are ANDed together, meaning a request MUST match all the specified headers to select the route. |
| GR-30 | GRPCMethodMatch | grpcroute_types.go:335 | MUST | At least one of Service and Method MUST be a non-empty string. |
| GR-33 | GRPCMethodMatchType | grpcroute_types.go:377 | MUST | Exact methods MUST be syntactically valid: must not contain `/` character. |
| GR-34 | GRPCHeaderMatch.Name | grpcroute_types.go:410 | MUST | If multiple entries specify equivalent header names, only the first entry with an equivalent name MUST be considered for a match. |
| GR-35 | GRPCHeaderMatch.Name | grpcroute_types.go:411 | MUST | Subsequent entries with an equivalent header name MUST be ignored. |
| GR-36 | GRPCHeaderMatchType | grpcroute_types.go:434 | MUST | Implementations MUST ensure that unknown values will not cause a crash. |
| GR-37 | GRPCHeaderMatchType | grpcroute_types.go:436 | MUST | Unknown values here MUST result in the implementation setting the Accepted Condition for the Route to `status: False`, with a Reason of `UnsupportedValue`. |
| GR-38 | GRPCRouteFilterRequestMirror | grpcroute_types.go:472 | MUST | The responses from this backend MUST be ignored by the Gateway. |
| GR-39 | GRPCRouteFilter.Type | grpcroute_types.go:509 | MUST | All implementations supporting GRPCRoute MUST support core filters. |
| GR-40 | GRPCRouteFilter.Type | grpcroute_types.go:520 | MUST | Type MUST be set to "ExtensionRef" for custom filters. |
| GR-41 | GRPCRouteFilter.Type | grpcroute_types.go:526 | MUST NOT | If a reference to a custom filter type cannot be resolved, the filter MUST NOT be skipped. |
| GR-42 | GRPCRouteFilter.Type | grpcroute_types.go:527 | MUST | Instead, requests that would have been processed by that filter MUST receive a HTTP error response. |
| GR-43 | GRPCRouteFilter.ExtensionRef | grpcroute_types.go:569 | MUST NOT | ExtensionRef MUST NOT be used for core and extended filters. |
| GR-44 | GRPCBackendRef | grpcroute_types.go:588 | SHOULD | When the BackendRef points to a Kubernetes Service, implementations SHOULD honor the appProtocol field if it is set for the target Service Port. |
| GR-45 | GRPCBackendRef | grpcroute_types.go:591 | SHOULD | Implementations supporting appProtocol SHOULD recognize the Kubernetes Standard Application Protocols defined in KEP-3726. |
| GR-46 | GRPCBackendRef | grpcroute_types.go:594 | MAY | If a Service appProtocol isn't specified, an implementation MAY infer the backend protocol through its own means. |
| GR-48 | GRPCBackendRef | grpcroute_types.go:598 | MUST | If a Route is not able to send traffic to the backend using the specified protocol then the backend is considered invalid. Implementations MUST set the "ResolvedRefs" condition to "False" with the "UnsupportedProtocol" reason. |
| GR-49 | GRPCBackendRef.BackendRef | grpcroute_types.go:606 | MUST | In all cases, the implementation MUST ensure the `ResolvedRefs` Condition on the Route is set to `status: False`, with a Reason and Message that indicate the cause of the error. |
| GR-50 | GRPCBackendRef.BackendRef | grpcroute_types.go:613 | MUST | When it refers to an unknown or unsupported kind of resource, the Reason MUST be set to `InvalidKind` and Message MUST explain which kind is unknown or unsupported. |
| GR-51 | GRPCBackendRef.BackendRef | grpcroute_types.go:617 | MUST | When it refers to a resource that does not exist, the Reason MUST be set to `BackendNotFound` and the Message MUST explain which resource does not exist. |
| GR-52 | GRPCBackendRef.BackendRef | grpcroute_types.go:623 | MUST | When it refers to a resource in another namespace without an explicit ReferenceGrant, the Reason MUST be set to `RefNotPermitted` and the Message MUST explain which cross-namespace reference is not allowed. |
| GR-53 | GRPCBackendRef.Filters | grpcroute_types.go:643 | MUST | Filters defined at this level MUST be executed if and only if the request is being forwarded to the backend defined here. |
| GR-54 | GRPCBackendRef.BackendRef | grpcroute_types.go:626 | MUST (via GR-49) | A BackendRef is invalid when the BackendTLSPolicy object is installed in the cluster, a BackendTLSPolicy is present that refers to the Service, and the implementation is unable to meet the requirement; GR-49 then requires ResolvedRefs=False with a Reason and Message. (Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |

## Shared types (shared_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| SH-06 | ParentReference.SectionName | shared_types.go:102 | MAY | Implementations MAY choose to support attaching Routes to other resources. |
| SH-07 | ParentReference.SectionName | shared_types.go:103 | MUST | If that is the case, they MUST clearly document how SectionName is interpreted. |
| SH-08 | ParentReference.SectionName | shared_types.go:110 | MUST | If 1 of 2 Gateway listeners accept attachment from the referencing Route, the Route MUST be considered successfully attached. |
| SH-09 | ParentReference.SectionName | shared_types.go:112 | MUST | If no Gateway listeners accept attachment from this Route, the Route MUST be considered detached from the Gateway. |
| SH-12 | ParentReference.Port | shared_types.go:137 | MAY | Implementations MAY choose to support other parent resources. |
| SH-13 | ParentReference.Port | shared_types.go:138 | MUST | Implementations supporting other types of parent resources MUST clearly document how/if Port is interpreted. |
| SH-14 | ParentReference.Port | shared_types.go:144 | MUST | If 1 of 2 Gateway listeners accept attachment from the referencing Route, the Route MUST be considered successfully attached. |
| SH-15 | ParentReference.Port | shared_types.go:146 | MUST | If no Gateway listeners accept attachment from this Route, the Route MUST be considered detached from the Gateway. |
| SH-16 | GatewayDefaultScope | shared_types.go:160 | MUST NOT | "None" is a special scope which explicitly means that the Route MUST NOT attach to any default Gateway. |
| SH-17 | GatewayDefaultScopeNone | shared_types.go:172 | MUST NOT | GatewayDefaultScopeNone indicates that a Gateway MUST NOT claim any Route asking for a default Gateway. |
| SH-18 | CommonRouteSpec | shared_types.go:177 | MUST | CommonRouteSpec defines the common attributes that all Routes MUST include within their spec. |
| SH-26 | BackendRef | shared_types.go:292 | SHOULD | When the BackendRef points to a Kubernetes Service, implementations SHOULD honor the appProtocol field if it is set for the target Service Port. |
| SH-27 | BackendRef | shared_types.go:295 | SHOULD | Implementations supporting appProtocol SHOULD recognize the Kubernetes Standard Application Protocols defined in KEP-3726. |
| SH-28 | BackendRef | shared_types.go:298 | MAY | If a Service appProtocol isn't specified, an implementation MAY infer the backend protocol through its own means. |
| SH-30 | BackendRef | shared_types.go:302 | MUST | If a Route is not able to send traffic to the backend using the specified protocol then the backend is considered invalid. Implementations MUST set the "ResolvedRefs" condition to "False" with the "UnsupportedProtocol" reason. |
| SH-33 | RouteConditionPartiallyInvalid | shared_types.go:448 | MUST | When this happens, implementations MUST take one of the following approaches. |
| SH-34 | RouteConditionPartiallyInvalid | shared_types.go:452 | MUST | The message for this condition MUST start with the prefix "Dropped Rule" and include information about which Rules have been dropped. |
| SH-35 | RouteConditionPartiallyInvalid | shared_types.go:454 | MUST | In this state, the "Accepted" condition MUST be set to "True" with the latest generation of the resource. |
| SH-36 | RouteConditionPartiallyInvalid | shared_types.go:452 | MUST | The message for this condition MUST start with the prefix "Fall Back" and include information about why the current Rule(s) are invalid. |
| SH-37 | RouteConditionPartiallyInvalid | shared_types.go:460 | MUST | To represent this, the "Accepted" condition MUST be set to "True" with the generation of the last known good state of the resource. |
| SH-39 | RouteConditionPartiallyInvalid | shared_types.go:468 | MUST NOT | This condition MUST NOT be set if a Route is fully valid, fully invalid, or not accepted. |
| SH-40 | RouteConditionPartiallyInvalid | shared_types.go:469 | MUST | This condition MUST only be set when it is "True". |
| SH-42 | RouteParentStatus.ControllerName | shared_types.go:499 | MUST | Controllers MUST populate this field when writing status. |
| SH-44 | RouteParentStatus.Conditions | shared_types.go:509 | MUST | If the Route's ParentRef specifies an existing Gateway that supports Routes of this kind AND that Gateway's controller has sufficient access, then that Gateway's controller MUST set the "Accepted" condition on the Route. |
| SH-45 | RouteParentStatus.Conditions | shared_types.go:515 | MUST | A Route MUST be considered "Accepted" if at least one of the Route's rules is implemented by the Gateway. |
| SH-46 | RouteParentStatus.Conditions | shared_types.go:535 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| SH-47 | RouteParentStatus.Conditions | shared_types.go:539 | MUST NOT | Implementations MUST NOT remove or reorder Conditions that they are not directly responsible for. |
| SH-48 | RouteParentStatus.Conditions | shared_types.go:540 | MUST NOT | If an implementation sees a Condition with type `special.io/SomeField`, it MUST NOT remove, change or update that Condition. |
| SH-49 | RouteParentStatus.Conditions | shared_types.go:543 | MUST | Implementations MUST always merge changes into Conditions of the same Type. |
| SH-50 | RouteParentStatus.Conditions | shared_types.go:545 | MUST | Implementations MUST always update the `observedGeneration` field of the Condition to the `metadata.generation` of the Gateway at the time of update creation. |
| SH-51 | RouteParentStatus.Conditions | shared_types.go:547 | MUST NOT | If the `observedGeneration` of a Condition is greater than the value the implementation knows about, then it MUST NOT perform the update on that Condition. |
| SH-53 | RouteStatus | shared_types.go:563 | MUST | RouteStatus defines the common attributes that all Routes MUST include within their status. |
| SH-54 | RouteStatus.Parents | shared_types.go:587 | MUST | Parent status MUST be considered to be namespaced by the combination of the parentRef and controllerName fields. |
| SH-56 | RouteStatus.Parents | shared_types.go:591 | MUST | Implementations MUST update only entries that have a matching value of `controllerName` for that implementation. |
| SH-57 | RouteStatus.Parents | shared_types.go:593 | MUST NOT | Implementations MUST NOT update entries with non-matching `controllerName` fields. |
| SH-58 | RouteStatus.Parents | shared_types.go:595 | MUST | Implementations MUST treat each `parentRef` in the Route separately and update its status based on the relationship with that parent. |
| SH-59 | RouteStatus.Parents | shared_types.go:597 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| SH-63 | AbsoluteURI | shared_types.go:645 | MUST NOT | The AbsoluteURI MUST NOT be a relative URI. |
| SH-64 | AbsoluteURI | shared_types.go:645 | MUST | The AbsoluteURI MUST follow the URI syntax and encoding rules specified in RFC3986. |
| SH-65 | AbsoluteURI | shared_types.go:646 | MUST | The AbsoluteURI MUST include both a scheme (e.g., "http" or "spiffe") and a scheme-specific-part. |
| SH-66 | AbsoluteURI | shared_types.go:647 | MUST | URIs that include an authority MUST include a fully qualified domain name or IP address as the host. |
| SH-67 | CORSOrigin | shared_types.go:656 | MUST NOT | The CORSOrigin MUST NOT be a relative URI. |
| SH-68 | CORSOrigin | shared_types.go:645 | MUST | The CORSOrigin MUST follow the URI syntax and encoding rules specified in RFC3986. |
| SH-69 | CORSOrigin | shared_types.go:657 | MUST | The CORSOrigin MUST include both a scheme ("http" or "https") and a scheme-specific-part, or it should be a single '*' character. |
| SH-71 | CORSOrigin | shared_types.go:647 | MUST | URIs that include an authority MUST include a fully qualified domain name or IP address as the host. |

### shared_types descriptive/CRD-enforced rows (lowercase forms — non-normative)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| SH-19 | CommonRouteSpec.ParentRefs | shared_types.go:199 | must (lc) | ParentRefs must be distinct (multi-part key of group, kind, namespace, name unique across all parentRef entries). |
| SH-31 | RouteConditionAccepted | shared_types.go:361 | should (lc) | Controllers may raise this condition with other reasons, but should prefer to use the reasons listed above to improve interoperability. |
| SH-32 | RouteConditionResolvedRefs | shared_types.go:414 | should (lc) | Controllers may raise this condition with other reasons, but should prefer to use the reasons listed above. |
| SH-38 | RouteConditionPartiallyInvalid | shared_types.go:464 | should (lc) | Reverting to the last known good state should only be done by implementations that have a means of restoring that state if/when they are restarted. |
| SH-43 | RouteParentStatus.ControllerName | shared_types.go:499 | should (lc) | Controllers should ensure that entries to status populated with their ControllerName are cleaned up when they are no longer necessary. |
| SH-77 | SessionPersistence.SessionName | shared_types.go:919 | should (lc) | Users should avoid reusing session names to prevent unintended consequences, such as rejection or unpredictable behavior. |
| SH-78 | RouteConditionAccepted | shared_types.go:357 | may/should (lc) | Possible reasons for this condition to be Unknown are: "Pending". Controllers may raise this condition with other reasons, but should prefer to use the reasons listed above to improve interoperability. (Pending itself: "used with the Accepted when a controller has not yet reconciled the route", shared_types.go:389-391. Captured for the Status=False pairing this controller uses; appended out of line order to keep pre-existing IDs stable.) |

## GatewayClass (gatewayclass_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| GC-01 | GatewayClass | gatewayclass_types.go:42 | MUST | If implementations choose to propagate GatewayClass changes to existing Gateways, that MUST be clearly documented by the implementation. |
| GC-02 | GatewayClass | gatewayclass_types.go:45 | SHOULD | Whenever one or more Gateways are using a GatewayClass, implementations SHOULD add the `gateway-exists-finalizer.gateway.networking.k8s.io` finalizer on the associated GatewayClass. |
| GC-03 | GatewayClass.Status | gatewayclass_types.go:65 | MUST | Implementations MUST populate status on all GatewayClass resources which specify their controller name. |
| GC-04 | GatewayClassSpec.ControllerName | gatewayclass_types.go:83 | MUST | The value of this field MUST be a domain prefixed path. |
| GC-05 | GatewayClassSpec.ParametersRef | gatewayclass_types.go:103 | SHOULD | If the referent cannot be found, refers to an unsupported kind, or when the data within that resource is malformed, the GatewayClass SHOULD be rejected with the "Accepted" status condition set to "False" and an "InvalidParameters" reason. |
| GC-06 | ParametersReference.Namespace | gatewayclass_types.go:143 | MUST | This field is required when referring to a Namespace-scoped resource and MUST be unset when referring to a Cluster-scoped resource. |
| GC-07 | GatewayClassConditionStatusAccepted | gatewayclass_types.go:163 | MUST | This condition defaults to Unknown, and MUST be set by a controller when it sees a GatewayClass using its controller string. |
| GC-08 | GatewayClassConditionStatusAccepted | gatewayclass_types.go:164 | MUST | The status of this condition MUST be set to True if the controller will support provisioning Gateways using this class. |
| GC-09 | GatewayClassConditionStatusAccepted | gatewayclass_types.go:166 | MUST | Otherwise, this status MUST be set to False. |
| GC-10 | GatewayClassConditionStatusAccepted | gatewayclass_types.go:167 | SHOULD | If the status is set to False, the controller SHOULD set a Message and Reason as an explanation. |
| GC-11 | GatewayClassConditionStatusSupportedVersion | gatewayclass_types.go:218 | MUST | This condition MUST be set by a controller when it marks a GatewayClass "Accepted". |
| GC-12 | GatewayClassConditionStatusSupportedVersion | gatewayclass_types.go:222 | MUST | If implementations detect any Gateway API CRDs that do not have this annotation set, or have it set to a version not recognized or supported, this condition MUST be set to false. |
| GC-13 | GatewayClassConditionStatusSupportedVersion | gatewayclass_types.go:227 | MAY | Implementations MAY choose to provide "best effort" support when an unrecognized CRD version is present. |
| GC-14 | GatewayClassConditionStatusSupportedVersion | gatewayclass_types.go:232 | MAY | Alternatively, implementations MAY choose not to support CRDs with unrecognized versions. |
| GC-15 | GatewayClassReasonUnsupportedVersion | gatewayclass_types.go:255 | SHOULD | A message SHOULD be included in this condition that includes the detected CRD version(s) and the supported CRD version(s). |
| GC-16 | GatewayClassStatus.Conditions | gatewayclass_types.go:278 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| GC-17 | GatewayClassStatus.Conditions | gatewayclass_types.go:282 | MUST NOT | Implementations MUST NOT remove or reorder Conditions that they are not directly responsible for. |
| GC-19 | GatewayClassStatus.Conditions | gatewayclass_types.go:286 | MUST | Implementations MUST always merge changes into Conditions of the same Type. |
| GC-20 | GatewayClassStatus.Conditions | gatewayclass_types.go:288 | MUST | Implementations MUST always update the `observedGeneration` field of the Condition to the `metadata.generation` of the Gateway at the time of update creation. |
| GC-21 | GatewayClassStatus.Conditions | gatewayclass_types.go:290 | MUST NOT | If the `observedGeneration` of a Condition is greater than the value the implementation knows about, then it MUST NOT perform the update on that Condition. |
| GC-22 | GatewayClassStatus.SupportedFeatures | gatewayclass_types.go:306 | MUST | SupportedFeatures MUST be sorted in ascending alphabetical order by the Name key. |
| GC-23 | supportedFeatureInternal | gatewayclass_types_overrides.go:51 | SHOULD NOT | This is solely for the purpose of ensuring backward compatibility and SHOULD NOT be used elsewhere. |
| GC-24 | GatewayClass | gatewayclass_types.go:52 | SHOULD | A GatewayClass name SHOULD be compliant with RFC 1035, consisting of a maximum of 63 lower case alphanumeric characters or hyphens ('-'). (New in the v1.6.0 godoc, missed by the v1.6.0 refresh. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |
| GC-25 | GatewayClass | gatewayclass_types.go:53 | MUST | A GatewayClass name MUST start and end with an alphanumeric character. (New in the v1.6.0 godoc, missed by the v1.6.0 refresh. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |

## ReferenceGrant (referencegrant_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| RG-01 | ReferenceGrant | referencegrant_types.go:38 | MUST NOT | Implementations that support ReferenceGrant MUST NOT permit cross-namespace references which have no grant. |
| RG-02 | ReferenceGrant | referencegrant_types.go:38 | MUST | Implementations that support ReferenceGrant MUST respond to the removal of a grant by revoking the access that the grant allowed. |
| RG-03 | ReferenceGrantSpec.From | referencegrant_types.go:68 | MUST | Each entry in the From list MUST be considered to be an additional place that references can be valid from; entries MUST be combined using OR. |
| RG-05 | ReferenceGrantSpec.To | referencegrant_types.go:81 | MUST | Each entry in the To list MUST be considered to be an additional place that references can be valid to; entries MUST be combined using OR. |
| RG-06 | ReferenceGrant.Spec | v1/referencegrant_types.go:48 (v1.6.0) | required (marker) | `spec` is now a `+required` field on ReferenceGrant in both served versions (kubernetes-sigs/gateway-api#4845, Standard channel, breaking at admission). Schema marker, not RFC-2119 prose — CRD-enforced, no controller obligation. v1.6.0 also moved the canonical ReferenceGrant Go types to `apis/v1` with `v1beta1` (storage) and `v1alpha2` (deprecated) as aliases. |

## object_reference (object_reference_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| OR-01 | LocalObjectReference | object_reference_types.go:24 | MUST (lc) | References to objects with invalid Group and Kind are not valid, and must be rejected by the implementation, with appropriate Conditions set on the containing object. |
| OR-02 | SecretObjectReference | object_reference_types.go:48 | MUST (lc) | References to objects with invalid Group and Kind are not valid, and must be rejected by the implementation, with appropriate Conditions set on the containing object. |
| OR-03 | BackendObjectReference.Kind | object_reference_types.go:120 | SHOULD NOT | Implementations SHOULD NOT support ExternalName Services (see CVE-2021-25740). |
| OR-04 | BackendObjectReference | object_reference_types.go:95 | MUST (lc) | References to objects with invalid Group and Kind are not valid, and must be rejected by the implementation, with appropriate Conditions set on the containing object. |
| OR-05 | ObjectReference | object_reference_types.go:165 | MUST (lc) | References to objects with invalid Group and Kind are not valid, and must be rejected by the implementation, with appropriate Conditions set on the containing object. |

## BackendTLSPolicy (backendtlspolicy_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| BTLS-01 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:72 | MUST | When more than one BackendTLSPolicy selects the same target and sectionName, implementations MUST determine precedence using the following criteria, continuing on ties. |
| BTLS-02 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:76 | MUST | The older policy by creation timestamp MUST be given precedence. |
| BTLS-03 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:84 | MUST | For any BackendTLSPolicy that does not take precedence, the implementation MUST ensure the `Accepted` Condition is set to `status: False`, with Reason `Conflicted`. |
| BTLS-04 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:88 | SHOULD NOT | Implementations SHOULD NOT support more than one targetRef at this time. |
| BTLS-05 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:100 | MAY | Implementations MAY use BackendTLSPolicy for Services not referenced by any Route, Gateway feature backends, service mesh, and other resource types beyond Service. |
| BTLS-06 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:105 | SHOULD | Implementations SHOULD aim to ensure that BackendTLSPolicy behavior is consistent, even outside of the extended HTTPRoute->Service path. |
| BTLS-07 | BackendTLSPolicySpec.TargetRefs | backendtlspolicy_types.go:107 | SHOULD | They SHOULD clearly document how BackendTLSPolicy is interpreted in these scenarios. |
| BTLS-08 | BackendTLSPolicySpec.Options | backendtlspolicy_types.go:138 | MAY | A set of common keys MAY be defined by the API in the future. |
| BTLS-09 | BackendTLSPolicySpec.Options | backendtlspolicy_types.go:138 | MUST | To avoid any ambiguity, implementation-specific definitions MUST use domain-prefixed names. |
| BTLS-12 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:160 | MUST | If CACertificateRefs is empty or unspecified, the configuration for WellKnownCACertificates MUST be honored instead if supported by the implementation. |
| BTLS-13 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:177 | MAY | Implementations MAY choose to perform further validation of the certificate content. |
| BTLS-14 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:181 | MUST | In all cases, the implementation MUST ensure the `ResolvedRefs` Condition on the BackendTLSPolicy is set to `status: False`, with a Reason and Message that indicate the cause of the error. |
| BTLS-15 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:183 | MUST | Connections using an invalid CACertificateRef MUST fail, and the client MUST receive an HTTP 5xx error response. |
| BTLS-16 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:185 | MUST | If ALL CACertificateRefs are invalid, the implementation MUST also ensure the `Accepted` Condition is set to `status: False`, with a Reason `NoValidCACertificate`. |
| BTLS-17 | BackendTLSPolicyValidation.CACertificateRefs | backendtlspolicy_types.go:191 | MAY | Implementations MAY choose to support attaching multiple certificates to a backend, but this behavior is implementation-specific. |
| BTLS-20 | BackendTLSPolicyValidation.WellKnownCACertificates | backendtlspolicy_types.go:211 | MUST | If an implementation does not support WellKnownCACertificates, or the supplied value is not recognized, the implementation MUST ensure the `Accepted` Condition is set to `status: False`, with a Reason `Invalid`. |
| BTLS-21 | BackendTLSPolicyValidation.WellKnownCACertificates | backendtlspolicy_types.go:219 | MAY | Implementations MAY define their own sets of CA certificates. |
| BTLS-22 | BackendTLSPolicyValidation.WellKnownCACertificates | backendtlspolicy_types.go:219 | MUST | Such definitions MUST use an implementation-specific, prefixed name. |
| BTLS-23 | BackendTLSPolicyValidation.Hostname | backendtlspolicy_types.go:231 | MUST | Hostname MUST be used as the SNI to connect to the backend (RFC 6066). |
| BTLS-24 | BackendTLSPolicyValidation.Hostname | backendtlspolicy_types.go:232 | MUST | Hostname MUST be used for authentication and MUST match the certificate served by the matching backend, unless SubjectAltNames is specified. |
| BTLS-25 | BackendTLSPolicyValidation.Hostname | backendtlspolicy_types.go:234 | MUST NOT | If SubjectAltNames are specified, Hostname can be used for certificate selection but MUST NOT be used for authentication. |
| BTLS-26 | BackendTLSPolicyValidation.Hostname | backendtlspolicy_types.go:235 | MUST | If you want to use the value of the Hostname field for authentication, you MUST add it to the SubjectAltNames list. |
| BTLS-27 | BackendTLSPolicyValidation.SubjectAltNames | backendtlspolicy_types.go:244 | MUST | When specified, the certificate served from the backend MUST have at least one Subject Alternate Name matching one of the specified SubjectAltNames. |
| BTLS-28 | SubjectAltName.URI | backendtlspolicy_types.go:277 | MUST | It MUST include both a scheme (e.g., "http" or "ftp") and a scheme-specific-part. |

## Policy attachment (policy_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| POL-01 | PolicyLabelKey | policy_types.go:23 | SHOULD | The value of the label SHOULD be one of the following. |
| POL-02 | NamespacedPolicyTargetReference.Namespace | policy_types.go:73 | MUST | Even when policy targets a resource in a different namespace, it MUST only apply to traffic originating from the same namespace as the policy. |
| POL-03 | PolicyAncestorStatus | policy_types.go:158 | SHOULD | Implementations SHOULD use Gateway as the PolicyAncestorStatus object unless the designers have a very good reason otherwise. |
| POL-04 | PolicyAncestorStatus | policy_types.go:175 | SHOULD | For objects where the parent is the relevant object for status, this struct SHOULD still be used. |
| POL-05 | PolicyAncestorStatus.ControllerName | policy_types.go:196 | MUST | Controllers MUST populate this field when writing status. |
| POL-06 | PolicyAncestorStatus.Conditions | policy_types.go:214 | MUST | Implementations MUST perform a read-modify-write cycle on this field before modifying it. |
| POL-07 | PolicyAncestorStatus.Conditions | policy_types.go:218 | MUST NOT | Implementations MUST NOT remove or reorder Conditions that they are not directly responsible for. |
| POL-09 | PolicyAncestorStatus.Conditions | policy_types.go:222 | MUST | Implementations MUST always merge changes into Conditions of the same Type. |
| POL-10 | PolicyAncestorStatus.Conditions | policy_types.go:224 | MUST | Implementations MUST always update the `observedGeneration` field of the Condition to the `metadata.generation` of the Gateway at the time of update creation. |
| POL-11 | PolicyAncestorStatus.Conditions | policy_types.go:226 | MUST NOT | If the `observedGeneration` of a Condition is greater than the value the implementation knows about, then it MUST NOT perform the update on that Condition. |
| POL-12 | PolicyStatus.Ancestors | policy_types.go:247 | MUST | When this policy attaches to a parent, the controller that manages the parent and the ancestors MUST add an entry to this list when the controller first sees the policy. |
| POL-13 | PolicyStatus.Ancestors | policy_types.go:249 | SHOULD | The controller SHOULD update the entry as appropriate when the relevant ancestor is modified. |
| POL-14 | PolicyStatus.Ancestors | policy_types.go:256 | MUST | Implementations MUST ONLY populate ancestor status for the Ancestor resources they are responsible for. |
| POL-15 | PolicyStatus.Ancestors | policy_types.go:257 | MUST | Implementations MUST use the ControllerName field to uniquely identify the entries in this list that they are responsible for. |
| POL-16 | PolicyStatus.Ancestors | policy_types.go:261 | MUST | The list of PolicyAncestorStatus structs MUST be treated as a map with a composite key, made up of the AncestorRef and ControllerName fields combined. |
| POL-17 | PolicyStatus.Ancestors | policy_types.go:268 | MUST NOT | If this slice is full, implementations MUST NOT add further entries. |
| POL-18 | PolicyStatus.Ancestors | policy_types.go:269 | MUST | Instead they MUST consider the policy unimplementable and signal that on any related resources. |
| POL-19 | PolicyAncestorStatus.ControllerName | policy_types.go:196 | should (lc) | Controllers should ensure that entries to status populated with their ControllerName are cleaned up when they are no longer necessary. (Policy twin of SH-43. Added in the v1.6.2 sync; appended out of line order to keep pre-existing IDs stable.) |

## ListenerSet (listenerset_types.go)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| LS-01 | ListenerSet | listenerset_types.go:35 | MUST (lc) | The parent Gateway must explicitly allow ListenerSet attachment through its AllowedListeners configuration. |
| LS-02 | ListenerSet | listenerset_types.go:46 | SHOULD (lc) | If an implementation cannot apply a policy to specific listeners, it should reject the policy. |
| LS-03 | ListenerSetSpec.Listeners | listenerset_types.go:90 | MUST | Implementations MUST treat the parent Gateway as having the merged list of all listeners using precedence: parent Gateway, then ListenerSet by creation time (oldest first), then alphabetically by namespace/name. |
| LS-04 | ListenerSetSpec.Listeners | listenerset_types.go:98 | MAY | An implementation MAY reject listeners by setting the ListenerEntryStatus `Accepted` condition to False with the Reason `TooManyListeners`. |
| LS-05 | ListenerSetSpec.Listeners | listenerset_types.go:104 | SHOULD | Implementations SHOULD be cautious about what information from the parent or siblings are reported to avoid accidentally leaking sensitive information. |
| LS-06 | ListenerEntry.Name | listenerset_types.go:124 | MUST | Name is the name of the Listener. This name MUST be unique within a ListenerSet. |
| LS-07 | ListenerEntry.Hostname | listenerset_types.go:138 | MUST | Implementations MUST apply Hostname matching appropriately for each of the following protocols. |
| LS-08 | ListenerEntry.Hostname | listenerset_types.go:141 | MUST | TLS: The Listener Hostname MUST match the SNI. |
| LS-09 | ListenerEntry.Hostname | listenerset_types.go:142 | MUST | HTTP: The Listener Hostname MUST match the Host header of the request. |
| LS-10 | ListenerEntry.Hostname | listenerset_types.go:143 | SHOULD | HTTPS: The Listener Hostname SHOULD match at both the TLS and HTTP protocol layers as described above. |
| LS-11 | ListenerEntry.Hostname | listenerset_types.go:144 | MUST | If an implementation does not ensure that both the SNI and Host header match the Listener hostname, it MUST clearly document that. |
| LS-12 | ListenerEntry.Hostname | listenerset_types.go:148 | MUST | For HTTPRoute and TLSRoute resources, when both listener and route specify hostnames, there MUST be an intersection between the values for a Route to be accepted. |
| LS-13 | ListenerEntry.TLS | listenerset_types.go:181 | MUST | The GatewayClass MUST use the longest matching SNI out of all available certificates for any TLS handshake. |
| LS-14 | ListenerEntry.AllowedRoutes | listenerset_types.go:187 | MAY | AllowedRoutes defines the types of routes that MAY be attached to a Listener and the trusted namespaces where those Route resources MAY be present. |
| LS-15 | ListenerEntry.AllowedRoutes | listenerset_types.go:192 | MUST | Matching precedence MUST be determined in order: most specific match by Route type, oldest Route by creation timestamp, then first alphabetically by namespace/name. |
| LS-19 | ListenerSetStatus.Conditions | listenerset_types.go:218 | MUST | Implementations MUST express ListenerSet conditions using the `ListenerSetConditionType` and `ListenerSetConditionReason` constants. |
| LS-20 | ListenerEntryStatus.SupportedKinds | listenerset_types.go:251 | MUST | This MUST represent the kinds supported by an implementation for that Listener configuration. |
| LS-21 | ListenerEntryStatus.SupportedKinds | listenerset_types.go:254 | MUST NOT | If kinds are specified in Spec that are not supported, they MUST NOT appear in this list. |
| LS-22 | ListenerEntryStatus.SupportedKinds | listenerset_types.go:254 | MUST | If kinds are specified in Spec that are not supported, an implementation MUST set the "ResolvedRefs" condition to "False" with the "InvalidRouteKinds" reason. |
| LS-23 | ListenerEntryStatus.SupportedKinds | listenerset_types.go:256 | MUST | If both valid and invalid Route kinds are specified, the implementation MUST reference the valid Route kinds that have been specified. |
| LS-24 | ListenerEntryStatus.AttachedRoutes | listenerset_types.go:276 | MUST | The AttachedRoutes field count MUST be set for Listeners, even if the Accepted condition of an individual Listener is set to "False". |
| LS-25 | ListenerEntryStatus.AttachedRoutes | listenerset_types.go:280 | MUST NOT | Routes with any other value for the Accepted condition MUST NOT be included in this count. |
| LS-35 | ListenerEntryReasonInvalidCertificateRef | listenerset_types.go:535 | MUST (lc) | This reason must be used only when the reference is allowed (same namespace or via ReferenceGrant). |
| LS-36 | ListenerEntryReasonInvalidCertificateRef | listenerset_types.go:538 | MUST (lc) | If the reference is not allowed, the reason RefNotPermitted must be used instead. |
| LS-40 | ListenerEntryConditionReady | listenerset_types.go:609 | SHOULD NOT (lc) | "Ready" is a condition type reserved for future use. It should not be used by implementations. |

## Exempt / secondary types (TLSRoute, TCPRoute, UDPRoute, v1alpha2, v1beta1)

| ID | Field/Type | file:line | Keyword | Requirement |
| --- | --- | --- | --- | --- |
| OTHER-01 | TLSRouteSpec | v1/tlsroute_types.go:51 | MUST | A TLSRoute MUST be attached to a Listener of protocol TLS. |
| OTHER-06 | TLSRouteSpec.Hostnames | v1/tlsroute_types.go:81 | MUST | If both the Listener and TLSRoute have specified hostnames, any TLSRoute hostnames that do not match any Listener hostname MUST be ignored. |
| OTHER-09 | TLSRouteSpec.Hostnames | v1/tlsroute_types.go:93 | MUST | A Listener MUST have protocol set to TLS when a TLSRoute attaches to it. |
| OTHER-10 | TLSRouteSpec.Hostnames | v1/tlsroute_types.go:93 | MUST | The implementation MUST raise an 'Accepted' Condition with a status of `False` with reason "UnsupportedValue" in case a Listener of the wrong type is used. |
| OTHER-11 | TLSRouteRule.Name | v1/tlsroute_types.go:126 | MUST | Name is the name of the route rule. This name MUST be unique within a Route if it is set. |
| OTHER-20 | TLSRouteSpec.Hostnames (v1alpha2) | v1alpha2/tlsroute_types.go:74 | MUST | If both the Listener and TLSRoute have specified hostnames, any TLSRoute hostnames that do not match the Listener hostname MUST be ignored. |
| OTHER-23 | TCPRouteRule.Name | v1/tcproute_types.go:67 | MUST | Name is the name of the route rule. This name MUST be unique within a Route if it is set. |
| OTHER-25 | TCPRouteRule.BackendRefs | v1/tcproute_types.go:74 | MUST | If unspecified or invalid, the underlying implementation MUST actively reject connection attempts to this backend. |
| OTHER-28 | UDPRouteRule.Name | v1/udproute_types.go:67 | MUST | Name is the name of the route rule. This name MUST be unique within a Route if it is set. |
| OTHER-30 | UDPRouteRule.BackendRefs | v1/udproute_types.go:74 | MUST | If unspecified or invalid, the underlying implementation MUST actively reject connection attempts to this backend. |
| OTHER-33 | ReferenceGrant (v1alpha2) | v1alpha2/referencegrant_types.go:47 | MUST NOT | Implementations that support ReferenceGrant MUST NOT permit cross-namespace references which have no grant. |
| OTHER-44 | GatewayClass (v1beta1) | v1beta1/gatewayclass_types.go:43 | MUST | If implementations choose to propagate GatewayClass changes to existing Gateways, that MUST be clearly documented by the implementation. |
| OTHER-45 | GatewayClass (v1beta1) | v1beta1/gatewayclass_types.go:46 | SHOULD | Whenever one or more Gateways are using a GatewayClass, implementations SHOULD add the finalizer on the associated GatewayClass. |
| OTHER-47 | ReferenceGrant (v1beta1) | v1beta1/referencegrant_types.go:43 | MUST NOT | Implementations that support ReferenceGrant MUST NOT permit cross-namespace references which have no grant. |

Note: v1beta1 GatewayClass/HTTPRoute/ReferenceGrant and v1alpha2 TLS/TCP/UDPRoute + ReferenceGrant largely duplicate the v1 normative text (storage-version aliases). They are listed once here for completeness; the audit assesses the v1 canonical clause and treats the version aliases as satisfied-by-the-same-code.

Channel note (v1.6.0): TCPRoute and UDPRoute went GA — canonical Go types now live in `apis/v1` (`v1/tcproute_types.go`, `v1/udproute_types.go`) and both ship in the Standard channel CRD bundle (kubernetes-sigs/gateway-api#4920, #4923). The v1alpha2 `file:line` refs above predate the move; the normative text is unchanged. The implementation status is unaffected: the tunnel data plane is HTTP(S)-only, so TCPRoute/UDPRoute remain unsupported/exempt (`rows-OTHER.md`) — but they are now present in the standard bundle a cluster installs, not experimental-only.
