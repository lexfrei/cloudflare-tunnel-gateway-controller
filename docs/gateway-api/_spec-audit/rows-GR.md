# GRPCRoute clause assessment (GR-*)

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| GR-01 | MUST | CTRL | MET | test/conformance/conformance_test.go:57 | SupportGRPCRoute is claimed, so all GRPCRoute MUSTs are in force; served by the in-process proxy. |
| GR-02 | MUST | N/A | NA | docs/gateway-api/limitations.md:96 | ALPN HTTP/2 negotiation for HTTPS listeners is owned by the Cloudflare edge, not this controller; the proxy receives requests over the tunnel from cloudflared. |
| GR-03 | MUST | N/A | NA | internal/controller/gateway_controller.go:816 | Per-protocol UnsupportedProtocol on listeners is handled, but ALPN negotiation itself is edge-owned; HTTPS listeners are served. |
| GR-04 | MAY | N/A | NA | — | HTTP/1 upgrade is edge-owned (Cloudflare); optional clause, not applicable to this data plane. |
| GR-05 | MUST | N/A | NA | docs/gateway-api/limitations.md:177 | Client-facing h2c is edge-owned; the proxy serves gRPC as HTTP/2 over the tunnel (auto-upgrades the tunnel transport to http2 when a GRPCRoute is present). |
| GR-06 | MUST | N/A | NA | internal/controller/gateway_controller.go:816 | Listener-protocol UnsupportedProtocol exists, but h2c-with-prior-knowledge negotiation is edge-owned. |
| GR-07 | MAY | N/A | NA | — | HTTP/1 upgrade is edge-owned; optional clause. |
| GR-08 | MUST | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:96 | Wildcard-label-first enforced by the upstream Hostname type pattern; proxy honours it (internal/routebinding/hostname.go:43). |
| GR-09 | MUST | CTRL | MET | internal/routebinding/binding.go:169 | Shared HostnamesIntersect gate; no intersection -> NoMatchingListenerHostname (Accepted=False). |
| GR-10 | MUST | CTRL | MET | internal/routebinding/hostname.go:14 | Non-matching route hostnames are ignored via intersection check; converter serves only intersecting hostnames. |
| GR-11 | MUST NOT | CTRL | MET | internal/routebinding/hostname.go:72 | wildcard matcher restricts test.example.net under *.example.com correctly (suffix + label check). |
| GR-12 | MUST NOT | CTRL | MET | internal/routebinding/binding.go:169 | No intersecting hostname -> RouteReasonNoMatchingListenerHostname, route not accepted. |
| GR-13 | MUST | CTRL | MET | internal/routebinding/binding.go:68 | makeBindingResult sets Accepted=False with the rejection reason on RouteParentStatus. |
| GR-14 | MUST | CTRL | MET | internal/controller/route_crosstype.go:43 | resolveCrossTypeConflicts accepts exactly one of an HTTP/GRPC pair: oldest by creationTimestamp, then alpha by ns/name. v1.6.0: the site docs relaxed this to MAY (kubernetes-sigs/gateway-api#4598) — enforcing/rejecting is now one of the permitted options, so the retained rejection stays compliant; dropping the rejection and serving both types on an intersecting hostname (which the shared proxy data plane is capable of) would ALSO now be spec-permitted, making the enforcement a product choice rather than an obligation. The v1.6.0 API godoc still carries the MUST wording (upstream inconsistency, see inventory GR-14). |
| GR-15 | MUST | CTRL | MET | internal/controller/route_crosstype.go:157 | markBindingConflicted flips loser bindings to Accepted=False / Reason=Conflicted. v1.6.0: rejection itself is now optional per the site docs (#4598); the Accepted=False stamping applies for as long as the implementation keeps rejecting, which it does. |
| GR-16 | MUST | CRD | PARTIAL | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:152 | Rule-name uniqueness is an Experimental-channel XValidation only; not enforced in the Standard CRD this controller ships against, and no controller-side check. |
| GR-17 | MUST | CTRL | MET | internal/proxy/router.go:576 | matchRules ORs the matches within a rule (matches either condition). |
| GR-18 | MUST | CTRL | MET | internal/proxy/router.go:618 | findMatchingIndex returns 0 (match-all) when a rule has no matches. |
| GR-19 | MUST | CTRL | MET | internal/proxy/router.go:510 | computePriority assigns precedence scores; rules sorted descending by priority, ties continue. |
| GR-20 | MUST | CTRL | MET | internal/controller/route_crosstype.go:43 | No HTTP/GRPC merging — cross-type conflict rejects one route entirely rather than merging rules. |
| GR-21 | MUST | CTRL | PARTIAL | internal/proxy/router.go:510 | Hostname (exact>wildcard>default) + path-type/length encode service/method specificity, method, header counts; service-vs-method char-count ranking is approximated via path length, not separate service/method tiers. |
| GR-22 | MUST | CTRL | MET | internal/proxy/converter.go:99 | sortRoutesByPrecedence orders flattened rules oldest-first then alpha ns/name, resolving cross-Route ties. |
| GR-23 | MUST | CTRL | MET | internal/proxy/router.go:565 | sortRulesByPrecedence stable-sorts equal-priority rules by ruleIndex (first matching rule wins). |
| GR-24 | MUST | CTRL | PARTIAL | internal/proxy/grpc_converter.go:140 | Core RequestHeaderModifier served; extended ResponseHeaderModifier served; RequestMirror (extended) NOT served, fails closed (HTTP 500) with UnsupportedValue. |
| GR-25 | MUST | CTRL | PARTIAL | internal/proxy/handler.go:529 | All-invalid-backend rule returns HTTP 500, not a gRPC UNAVAILABLE (code 14) trailer; HTTP 500 maps to gRPC UNKNOWN/2 client-side. |
| GR-26 | MUST | CTRL | PARTIAL | internal/proxy/handler.go:540 | Invalid GRPCBackendRef stays in pool marked UnavailableStatus=500; returns HTTP 500 rather than grpc-status UNAVAILABLE. |
| GR-27 | MUST | CTRL | PARTIAL | internal/proxy/grpc_converter.go:332 | Weighted pool preserves the invalid backend's traffic fraction (marked 500); status is HTTP 500, not gRPC UNAVAILABLE. |
| GR-29 | MUST | CTRL | MET | internal/proxy/matcher.go:203 | All header matchers ANDed in CompileMatch; a request must satisfy every header. |
| GR-30 | MUST | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:337 | XValidation requires service or method non-empty; converter treats both-empty as match-all defensively. |
| GR-33 | MUST | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:339 | Exact method/service syntactic validity enforced by CRD XValidation regex. |
| GR-34 | MUST | CRD | PARTIAL | vendor grpcroute_types.go:325 (listType=map/listMapKey=name) | Exact-duplicate header names are rejected at admission by listMapKey=name, so first-wins is structurally enforced. Residual: gRPC header names match case-insensitively, so case-variant names bypass the case-sensitive listMapKey and are ANDed — same negligible header-only edge as HR-41. |
| GR-35 | MUST | CRD | PARTIAL | as GR-34 | Subsequent exact-duplicate header entries cannot exist (listMapKey); only the case-variant residual remains. |
| GR-36 | MUST | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:440 | HeaderMatchType is an Enum (Exact;RegularExpression); unknown values rejected at admission, no crash. |
| GR-37 | MUST | CTRL | NA | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:440 | Enum closes unknown values at admission, so the runtime UnsupportedValue path is unreachable; CRD prevents the input. |
| GR-38 | MUST | CTRL | NA | internal/proxy/grpc_converter.go:146 | RequestMirror is not served (fails closed HTTP 500), so there is no mirror response to ignore; clause vacuous. |
| GR-39 | MUST | CTRL | MET | internal/proxy/grpc_converter.go:142 | Core filter (RequestHeaderModifier) supported via the shared header-modifier pipeline. |
| GR-40 | MUST | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:531 | Filter Type Enum + XValidations require ExtensionRef for custom filters. |
| GR-41 | MUST NOT | CTRL | MET | internal/proxy/grpc_converter.go:146 | ExtensionRef fails closed (not skipped) — sets UnavailableStatus=500 and UnsupportedValue diagnostic. |
| GR-42 | MUST | CTRL | MET | internal/proxy/handler.go:306 | Unresolvable-filter rule returns HTTP error (500) to matched requests via writeRuleUnavailable. |
| GR-43 | MUST NOT | CRD | MET | vendor/sigs.k8s.io/gateway-api/apis/v1/grpcroute_types.go:502 | XValidation forbids extensionRef on non-ExtensionRef filter types. |
| GR-44 | SHOULD | CTRL | MET | internal/proxy/grpc_converter.go:applyGRPCBackendTransport; internal/proxy/converter.go:isTLSAppProtocol | gRPC converter reads the Service port appProtocol (protocolResolver) and honours the only axis an HTTP/2 transport exposes — TLS-vs-cleartext: a TLS appProtocol (https / HTTPS / kubernetes.io/wss) with no BackendTLSPolicy fails the backend closed (502, ResolvedRefs=False / UnsupportedProtocol), same as the HTTP path. Other values keep h2c (correct for gRPC). #438. |
| GR-45 | SHOULD | CTRL | MET | internal/proxy/converter.go:isTLSAppProtocol; internal/proxy/grpc_appprotocol_failclosed_test.go | The TLS-bearing KEP-3726 value kubernetes.io/wss (and https) is now recognised on the gRPC path and drives the fail-closed decision; kubernetes.io/h2c is the native gRPC default. #438. |
| GR-46 | MAY | CTRL | MET | internal/proxy/grpc_converter.go:289 | Backend protocol inferred by own means: h2c by default, HTTP/2-over-TLS when a BackendTLSPolicy/ExternalBackend https scheme applies. |
| GR-48 | MUST | CTRL | NA | internal/proxy/grpc_converter.go:289 | gRPC always dials HTTP/2 (h2c or ALPN), which is the gRPC transport, so the "cannot use specified protocol" UnsupportedProtocol case does not arise for gRPC backends. |
| GR-49 | MUST | CTRL | MET | internal/controller/route_status.go:376 | buildResolvedRefsCondition sets ResolvedRefs=False with a specific Reason+Message for failed refs (shared HTTP/GRPC path). |
| GR-50 | MUST | CTRL | MET | internal/ingress/service_resolver.go:73 | Unsupported kind -> BackendRefError Reason=InvalidKind, surfaced on ResolvedRefs. |
| GR-51 | MUST | CTRL | MET | internal/ingress/service_resolver.go:190 | Nonexistent resource -> Reason=BackendNotFound, surfaced on ResolvedRefs. |
| GR-52 | MUST | CTRL | MET | internal/ingress/service_resolver.go:221 | Cross-namespace without ReferenceGrant -> Reason=RefNotPermitted; grpc-specific validator at proxy_syncer.go:105. |
| GR-53 | MUST | CTRL | MET | internal/proxy/grpc_converter.go:167 | applyGRPCBackendFilters attaches per-backend filters to the selected backend only (executed iff forwarded to that backend). |
