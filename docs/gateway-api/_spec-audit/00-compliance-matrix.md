# Gateway API v1.6.2 spec compliance matrix

Clause-by-clause audit of the implementation against the normative (RFC-2119) surface of `sigs.k8s.io/gateway-api v1.6.2` Standard channel, read from `vendor/` while that version was vendored. The full clause extraction and adversarial verification were performed at v1.5.1; the audit was then refreshed against the verified v1.5.1 → v1.6.0 tag diff (see "Baseline refreshes since v1.5.1" below) — v1.6.1 followed as a conformance/test-infrastructure-only patch release with no API or CRD changes (upstream v1.6.1 release notes) and v1.6.2 changed one godoc support level without touching the normative surface, so the v1.6.0 clause diff still covers every release absorbed here; the two object-name clauses it missed were rowed in the v1.6.2 sync below. Like its `rows-*.md` siblings, this file records an audit someone performed at a stated version. A dependency bump does not update it, so it can sit behind the vendored module until the next audit; comparing the version above against `go.mod` is how you tell. This is the deliverable the closed audit issue asked for: every implemented resource's normative clauses classified honoured / justified-deviation / violated, with code evidence.

## Method

1. Extracted every MUST / MUST NOT / SHOULD / SHOULD NOT / MAY clause from the vendored godoc into `01-clause-inventory.md`, by type. The counts here and in the dashboard are of the classified clause set in `rows-*.md`: 376 at v1.5.1, 378 after the v1.6.0 refresh added GW-106 and RG-06, 380 after GW-107 and SH-78 were added for the tunnel-ownership refusal, 381 after SH-48 rowed the second half of the foreign-condition clause, and 397 after the v1.6.2 sync added 16 rows for clauses the earlier passes left out (GW-108..GW-115, GC-24, GC-25, HR-73..HR-76, GR-54, POL-19). That set is the inventory plus the cross-cutting GEP-01..GEP-08 rows: the inventory holds the field-godoc extraction alone, and the GEP rows come from step 2's `02-gep-notes.md`.
2. Added cross-cutting GEP/concept requirements not in field godoc (policy attachment GEP-713, route-attachment semantics) — `02-gep-notes.md`.
3. Classified each clause CRD-enforced / controller-actionable / N/A-tunnel and assessed status MET / PARTIAL / GAP / NA against the real code — per-type detail in `rows-<TYPE>.md`.
4. Ran the official conformance suite (Gateway HTTP + gRPC profiles) against a fresh kind cluster + real Cloudflare test tunnel as pass/fail ground truth.
5. Adversarially re-verified every GAP — a skeptic tried to refute each (CRD enforcement, N/A, conditional-satisfied, documented-deviation) before it was allowed to stand. 18 of 25 first-pass GAPs did not survive.

## Dashboard (397 clauses: 376 from the v1.5.1 first-pass classification, 2 added by the v1.6.0 refresh — GW-106 MET, RG-06 NA — 2 covering the tunnel-ownership refusal — GW-107 MET, SH-78 GAP — SH-48 MET, and 16 added by the v1.6.2 sync — classified 11 MET, GW-112 PARTIAL, 4 NA)

Counts are the current `rows-*.md` verdicts (`cat rows-*.md | grep -E '^\| [A-Z]+-[0-9]+ \|' | awk -F'|' '{print $5}' | sort | uniq -c`); rows move as fixes land, so the table drifts from the first-pass split of 221 MET / 31 PARTIAL / 25 GAP / 99 N/A that this table carried when the matrix was first published. The 25 first-pass GAPs are traced under "Adversarial verification".

| Status | Count |
| --- | --- |
| MET | 281 |
| PARTIAL | 19 |
| GAP | 1 |
| REFUTED | 1 |
| DOWNGRADE-NA | 1 |
| DOWNGRADE-MET | 2 |
| DOWNGRADE-CONDITIONAL | 1 |
| DOWNGRADE-DOCUMENTED | 2 |
| N/A (tunnel architecture / exempt / MAY not taken) | 89 |

DOWNGRADE-DOCUMENTED marks a recommendation the controller does not follow on purpose, with the reason in limitations.md: OR-03 (ExternalName Services) and, since the v1.6.2 sync, BTLS-04 (a policy may list several targetRefs). It is the rows' spelling of DEVIATED-DOCUMENTED in the `shouldmay-*.md` tables.

Conformance ground truth (v1.5.1 run): 76 top-level subtests PASS, 54 SKIP (documented TLS/TCP/UDP/Mesh/WebSocket/GRPCRouteWeight/HTTPS-listener), **0 FAIL** (`go test ... ok 293s`). GRPCRouteWeight and HTTPRouteBackendProtocolWebSocket were among the SKIPs at that run; both are de-skipped in the current suite configuration (`test/conformance/conformance_test.go`, pinned by `TestStaleSkipsStayLifted`) now that gateway-api v1.6.0 added the injectable `suite.GRPCClient` / `suite.WebSocketDialer` hooks those tests needed, so the current skip categories are TLS/TCP/UDP/Mesh/HTTPS-listener plus the BackendTLSPolicy-gated tests. Conformance ground truth (v1.6.1 run): 77 top-level subtests PASS, 76 SKIP, **0 FAIL** (`go test ... ok 487s`, kind + real Cloudflare Tunnel). Both runs were green; the audit's value is the normative surface the suite does not exercise.

## v1.6.2 sync

The rows were re-read against the vendored v1.6.2 text and the code on master. File and line references became symbol references, since the line numbers had drifted. Verdict changes, each re-checked against the code:

- PARTIAL → MET: GR-24 (RequestMirror is Extended, the core filter is served), HR-21 and HR-24 (explicit `0s` disables the timeout, pinned by tests), HR-60 (Location carries no port, which is the scheme's well-known one), BTLS-06 (one resolver for HTTPRoute, GRPCRoute and mirror backends).
- NA → MET: GR-48 (a TLS appProtocol without a policy reports UnsupportedProtocol on the gRPC path), OTHER-45 (v1beta1 alias of GC-02).
- GAP → PARTIAL: HR-04, to match GR-16: documented, with an opt-in ValidatingAdmissionPolicy.
- PARTIAL → NA: GW-43 (its SNI premise is edge-side; the 404 it asks for is GW-106).
- MET → NA: GW-50, GW-51 (h2c on the client-facing listener is the edge's choice; the rows had read it as the backend hop).
- NA → PARTIAL: SH-19 (the Standard CRD enforces parentRef distinctness, but compares `namespace` literally, so an unset namespace and the route's own namespace count as different parents).
- NA → DOWNGRADE-DOCUMENTED: BTLS-04 (`shouldmay-BTLS.md` already had it as DEVIATED-DOCUMENTED).
- MET → PARTIAL: GW-88 and LS-35, with the new GW-112: a cross-namespace certificate reference of a kind other than Secret is reported InvalidCertificateRef (InvalidClientCertificateRef for the client certificate) before any grant check, where the clause asks for RefNotPermitted when no ReferenceGrant allows it.

Since the sync, GW-88, GW-112 and LS-35 moved PARTIAL → MET: the ReferenceGrant is checked before the kind, and only a grant naming the reference's own kind allows it.

Kept as recorded, pending a maintainer decision: GW-02 and GW-24 (listeners matched without their port), GW-101 and GW-103 (no OverlappingTLSConfig), GR-14 and GR-15 (HTTPRoute/GRPCRoute hostname conflict), SH-33 and SH-34 (an unparseable timeout or retry policy is reported as "Dropped Rule" while the rule keeps serving), SH-78 (Pending with Accepted=False), HR-62 (the scheme of a scheme-less redirect).

## Baseline refreshes since v1.5.1

The v1.6.0 baseline bump was audited against the verified upstream tag diff; v1.6.1 followed as a conformance/test-infrastructure-only patch (upstream v1.6.1 release notes: TCPRoute/UDPRoute conformance timeout and flake fixes, no API or CRD changes), and v1.6.2 followed with a single godoc support-level change (below). Every delta below cites the upstream PR; pre-existing verdicts stand unless a row carries an explicit v1.6.0 note.

| Delta | Upstream PR | Classification | Where it landed |
| --- | --- | --- | --- |
| `ReferenceGrant.spec` is REQUIRED in both served versions (breaking at admission) | kubernetes-sigs/gateway-api#4845 | CRD-enforced; no controller obligation. The validator (`internal/referencegrant/validator.go`) is fail-closed on an empty/missing spec anyway (nil `From`/`To` → no match → deny), so a legacy spec-less object persisted from before the upgrade cannot grant access. | Inventory RG-06; `rows-RG.md` RG-06. |
| Gateway listeners description: traffic matching no listener hostname MUST be rejected — HTTP 404, gRPC Unimplemented | kubernetes-sigs/gateway-api#4408 | New controller-actionable clause, MET on both halves: HTTP no-match returns 404, and an unmatched gRPC request gets a trailers-only response (HTTP 200 + grpc-status: 12) so the client observes Unimplemented. Verified on both response writers — standalone Go HTTP/2 (real grpc-go client) and the production cloudflared writer via the trailer bridge; a bare 404 is seen as Internal ("stream closed without trailers") over the tunnel, so the trailer is load-bearing. | Inventory GW-106; `rows-GW.md` GW-106. |
| `infrastructure.annotations` maxProperties 8 → 16 | kubernetes-sigs/gateway-api#4707 | CRD limit relaxation, informational. GW-84 (annotation propagation, per-Gateway plane) is a size-independent map copy — unaffected. | No row change. |
| `frontendValidation.caCertificateRefs` maxItems 8 → 16 | kubernetes-sigs/gateway-api#4088 | N/A — frontendValidation is not implemented (GW-63..GW-71 exempt, edge terminates TLS). | No row change. |
| Shared hostnames between HTTPRoute and GRPCRoute: site docs relaxed MUST-reject to MAY-reject | kubernetes-sigs/gateway-api#4598 | The controller's cross-type rejection (`internal/controller/route_crosstype.go`) was MET under the v1.5.1 MUST and remains compliant under the v1.6.0 MAY (enforcing is one of the permitted options); serving both route types on an intersecting hostname without rejection is now also spec-permitted, so the enforcement is a product choice, not an obligation. Upstream inconsistency: the v1.6.0 API godoc (`grpcroute_types.go` Hostnames) still carries the old MUST wording — candidate upstream docs issue. | Inventory GR-14/GR-15 notes; `rows-GR.md` GR-14/GR-15 notes; `shouldmay-GRSH.md` MAY row. |
| TCPRoute/UDPRoute went GA into the Standard channel | kubernetes-sigs/gateway-api#4920, #4923 | Channel inventory only — the tunnel data plane is HTTP(S)-only, so both remain unsupported/exempt; they now ship in the standard CRD bundle rather than experimental-only. | Inventory OTHER channel note; `rows-OTHER.md` header note. |
| `SessionPersistence.IdleTimeout` removed from the Go API | kubernetes-sigs/gateway-api#4771 | Experimental feature; SessionPersistence is unimplemented here and no audit row referenced IdleTimeout (SH-77 covers SessionName only). | No row change. |
| HTTPRoute Standard schema: NO changes | kubernetes-sigs/gateway-api#4639 (CORS repeated-filter CEL was already in v1.5.1), #4907 (retry validation is experimental-only; the Standard HTTPRoute CRD has no `retry` field in v1.6.0) | HR verdicts stand, including the HR-26..HR-39 retry block (then N/A, re-audited since, when the proxy gained Experimental-channel retry support). | `rows-HR.md` header note. |
| GRPCRoute / GatewayClass: doc-only changes | (tag diff) | No normative delta; verdicts stand. | No row change. |
| `HTTPRequestRedirectFilter.statusCode` support retiered: 301 and 302 Core, the other enum values Extended | (v1.6.2 tag diff) | Support-level reclassification only. The added godoc carries no RFC-2119 keyword and the `Enum=301;302;303;307;308` validation is unchanged, so no clause enters or leaves the inventory. No row cites redirect `statusCode` — every `statusCode` row in `01-clause-inventory.md`, `rows-HR.md` and `shouldmay-HR.md` is `HTTPRouteRetryStatusCode` in the retry block, so those files were checked against v1.6.2 and left at the version they were audited at; the retry rows have since been re-audited against v1.6.2. | No row change. |
| Gateway and GatewayClass object names: SHOULD be RFC 1035, MUST start and end with an alphanumeric character | (v1.6.0 tag diff) | Missed by the v1.6.0 refresh and rowed in the v1.6.2 sync. N/A: the apiserver validates custom resource names as DNS-1123 subdomains, so a name that breaks the MUST never reaches the controller. | GW-114, GW-115, GC-24, GC-25. |
| Well-known labels for generated resources (GEP-1762) | kubernetes-sigs/gateway-api#4705 | Lowercase must/should, rowed in the v1.6.2 sync as GW-108 (MET) and GW-109 (MET) — `apis/v1/well_known_labels.go` adds `gateway.networking.k8s.io/gateway-name` / `gateway-class-name` constants with lowercase must/should godoc (non-normative per the RFC-8174 caveat). Implemented: the per-Gateway rendered plane stamps both well-known keys on every rendered resource's metadata and on the Secrets generated for the plane (`internal/render/render.go` `ResourceLabels`) in addition to its own selector label (`cf.k8s.lex.la/gateway`); the Deployment selector itself stays controller-specific. | `02-gep-notes.md` GEP-16; GW-108, GW-109. |

## Adversarial verification: 25 first-pass GAPs → final verdicts

| Clauses | First pass | Final verdict | Basis |
| --- | --- | --- | --- |
| HR-41, HR-42, HR-43, HR-44, GR-34, GR-35 | GAP (duplicate match-name first-wins not honoured) | DOWNGRADE-CRD | `Headers`/`QueryParams` are `+listType=map`+`+listMapKey=name` (`vendor/sigs.k8s.io/gateway-api/apis/v1/httproute_types.go` `HTTPRouteMatch.Headers` / `QueryParams`, `grpcroute_types.go` `GRPCRouteMatch.Headers`); the API server rejects duplicate names at admission. Header names match case-insensitively, so case-variant names (`Foo`/`foo`) bypass the case-sensitive listMapKey and are ANDed rather than first-wins — a negligible header-only edge (doc note). Query-param names are exact-match per spec, so case-variants are legitimately distinct and there is no residual. |
| SH-47, SH-57 | GAP | **CONFIRMED (MUST NOT)** | At the first pass, route_status.go `updateRouteParentStatuses` set `Parents = nil` and a full `Status().Update` (not SSA) wiped other controllers' RouteParentStatus every reconcile; backendtlspolicy_controller.go `updateStatus` preserved foreign entries — route status does not. |
| SH-51, GC-21 | GAP (also GW-81, GW-100, POL-11 same class) | **CONFIRMED (MUST NOT), low** | No observedGeneration regression guard; status writers stamp `ObservedGeneration: generation` unconditionally. Get+RetryOnConflict guards resourceVersion only, not a stale-generation overwrite. Narrow race. |
| HR-04 (and GR-16) | GAP | **CONFIRMED (MUST), minor** | Rule-name uniqueness CEL is experimental-channel only (httproute_types.go `HTTPRouteSpec.Rules`); shipped Standard CRD strips it; no controller-side uniqueness check. |
| GC-05 | GAP | RESOLVED (was: CONFIRMED but **SHOULD**) | The GatewayClass reconciler now reports an unusable parametersRef (missing, unsupported group or kind, namespaced, or naming a GatewayClassConfig that does not exist) as Accepted=False/InvalidParameters, and re-evaluates the class when its config is created or deleted. |
| GC-02 | GAP | RESOLVED (was: CONFIRMED but **SHOULD**) | `gateway-exists-finalizer` is now managed by the GatewayClass reconciler (added while any Gateway uses the class, removed when none do). |
| GW-31, GW-87 | GAP | RESOLVED (was: DOWNGRADE-NA) | spec.addresses is never user-selectable for a tunnel, but a requested address it cannot serve is now reported: a non-Hostname type → Accepted=False/UnsupportedAddress, another hostname → Programmed=False/AddressNotUsable with a prescriptive message. |
| GW-75, GW-86 | GAP | DOWNGRADE-MET | Precondition is "if empty value NOT supported"; the controller supports empty (claims SupportGatewayAddressEmpty, always auto-assigns the tunnel CNAME), so the obligation is vacuously satisfied. |
| GC-09 | GAP | RESOLVED (was: DOWNGRADE-DEFENSIBLE) | Accepted is set False when the class cannot be served because its parametersRef is unusable (see GC-05). |
| GC-22 | GAP | DOWNGRADE-CONDITIONAL | Publishing `status.supportedFeatures` is optional; the "MUST be sorted" clause governs order only if published. Not published → vacuously satisfied. |
| SH-36 | GAP | REFUTED | The "Dropped Rule" PartiallyInvalid approach is implemented and tested (route_status.go `droppedConfigMessage`, `TestDiagnostics_SomeRulesDropped_PartiallyInvalid`); the spec requires only one of two approaches. |
| SH-43 | GAP | RESOLVED (was: DOWNGRADE-CONDITIONAL) | The controller removes its own entries from a route that no longer leads to a managed Gateway, including after its GatewayClass is deleted. |
| HR-61 | GAP | DOWNGRADE-NA | Redirect `Scheme` enum is http;https; both have well-known ports, so the "scheme without well-known port" precondition is unreachable. |
| GR-44, GR-45 | GAP | DOWNGRADE-NA | A GRPCRoute backend is gRPC-over-HTTP/2 by definition; forcing h2c is correct, and the one protocol-relevant signal (TLS via BackendTLSPolicy) is honoured. |
| OR-03 | GAP | DOWNGRADE-DOCUMENTED | ExternalName Service support is a deliberate, documented deviation (limitations.md "Controller Limitations", "Non-Service backend kinds") with a stated trust-boundary rationale that already cites CVE-2021-25740. |

## Confirmed findings (post-verification)

### Code bugs (file as kind/bug)

1. **Route status reconcile clobbers other controllers' `RouteParentStatus` (SH-47, SH-57; MUST NOT).** Fixed: `route_status.go` partitions the stored parents by controllerName, carries foreign entries over verbatim, and reserves their slots before truncating its own. The listener-status rebuild had the same shape and is fixed in `preserveConditionTransitions` on both the reconcile and config-error paths (GW-96/GW-98 MET).
2. **Status writers lack an observedGeneration regression guard (SH-51, GC-21, GW-81, GW-100, POL-11; MUST NOT).** Fixed: every status writer now runs `statusGenerationStale` / `ownedConditionsStale` (`internal/controller/status_generation.go`) or, for per-listener conditions, `ownedListenerConditionsStale`, and skips the write when a stored own condition already carries a newer observedGeneration.
3. **HTTPRoute/GRPCRoute rule-name uniqueness not enforced (HR-04, GR-16; MUST).** The uniqueness CEL is experimental-channel; the shipped Standard CRD omits it and the controller does not validate. Minor. Addressed with a documented limitation (limitations.md "Route rule name uniqueness (`spec.rules[].name`)") and an opt-in ValidatingAdmissionPolicy that enforces it at admission; no controller-side check, so both rows stay PARTIAL.

### Documentation additions (justified deviations, recorded in limitations.md by this change)

- spec.addresses accepts only the tunnel hostname (the tunnel address is not user-selectable; same basis as the exempt static-addresses feature); anything else is reported with UnsupportedAddress or AddressNotUsable — recorded in limitations.md "`spec.addresses` accepts only the tunnel hostname".
- ExternalName Service support now cites CVE-2021-25740 in its existing trust-boundary rationale.
- Case-variant duplicate header match names (`Foo` vs `foo`) bypass the case-sensitive CRD listMapKey and are ANDed rather than first-wins — negligible header-only edge (query-param names are exact-match, so unaffected); recorded under Route Conflict Resolution.
- A route bound only to a Gateway that cannot serve it reports `Accepted=False` with `Reason=Pending`, where the spec lists `Pending` under `Accepted=Unknown` for a route not yet reconciled (SH-78). The polarity is deliberate for the permanent causes — a refused tunnel claim, or a dedicated data plane whose resolve failed deterministically — since the condition stands until the Gateway is fixed and `Unknown` would read as "not looked at yet". The retryable cause, a transient resolve failure, carries the same reason without that justification, which is what keeps the row a GAP.

## SHOULD / MAY tiers (verified)

The SHOULD and MAY tiers were re-verified in a second pass after the MUST audit — per-type adversarial review for SHOULD, catalogue for MAY. Per-clause detail in `shouldmay-<TYPE>.md`.

### SHOULD / SHOULD NOT (53 clauses)

- HONOURED-TESTED (~22) and N/A for the tunnel architecture (~20) account for the bulk. The v1.6.2 sync added the two object-name SHOULDs, GW-114 and GC-24, both N/A.
- HONOURED-TESTED since the audit (was HONOURED-UNTESTED, 7): HR-21, HR-24, HR-63, BTLS-06, SH-31, SH-32, LS-05 — each now pinned by a regression test (explicit-zero timeouts, redirect Location port, BackendTLS HTTP/gRPC equivalence, reason-vocabulary AST guard, ListenerSet status leak guard).
- HONOURED-TESTED since the audit (was N/A, 3): HR-26 (except a request body that cannot be resent), HR-32, HR-34 — the proxy gained Experimental-channel retry support.
- DEVIATED-DOCUMENTED (5): BTLS-04, OR-03, POL-18, and GEP-08 and HR-61 from the next item — permitted deviations with a written rationale in limitations.md. POL-18 joined since the audit: past 16 ancestors each left-out Gateway gets a Warning Event, and the policy keeps applying to its traffic. GW-74, another before, was one and is now HONOURED-TESTED since the controller reports a spec.addresses type it cannot assign as UnsupportedAddress, and GW-82, GW-83, GW-84 and GW-87 moved to HONOURED-TESTED with the spec.addresses reporting and the labels on generated Secrets. SH-43, another at the audit, is HONOURED-TESTED since the controller removes its own status entries from routes that no longer lead to a managed Gateway. GC-05, another at the audit, is HONOURED-TESTED since the GatewayClass reconciler began reporting an unusable parametersRef as Accepted=False/InvalidParameters; GC-10 moved from N/A to HONOURED-TESTED with it.
- DEVIATED-SILENT (originally 3 distinct gaps across 4 clause IDs) — all resolved since the audit: GC-02 and its v1beta1 alias OTHER-45 are HONOURED (the reconciler now manages the gateway-exists-finalizer); GEP-08 (discoverability condition on the policy ancestor status, not the affected Gateway/Service) and HR-61 (no redirect-port fallback to the listener port — unreachable through the Standard CRD scheme enum http/https) are DEVIATED-DOCUMENTED with rationales in limitations.md. POL-11, which `shouldmay-BTLS.md` still listed as DEVIATED-SILENT, is HONOURED-TESTED: the observedGeneration guard covers policy ancestor entries too. Also resolved earlier: GR-44 / GR-45 (gRPC silently dialing cleartext when a Service declared a TLS appProtocol without a BackendTLSPolicy) now fails the backend closed, matching the HTTP path — #438.

### MAY (34 clauses)

Catalogued implemented / intentionally-omitted; zero worthwhile candidates surfaced. Every MAY is IMPLEMENTED, OMITTED-INTENTIONAL (edge-terminated TLS, status-only reconciler, single flattened ingress) or N-A: GW-50, h2c from clients on an HTTP listener, is the Cloudflare edge's choice. The optional surface is a deliberate product choice.

## Provenance

- `01-clause-inventory.md` — verbatim field-godoc clause extraction; the GEP rows are not in it (see Method step 1).
- `02-gep-notes.md` — GEP/concept cross-cutting requirements.
- `rows-<TYPE>.md` — per-clause classification + evidence (GW, HR, GR, SH, GC, RG, BTLS, LS, OTHER), holding the current verified verdict: a clause the adversarial pass fixed in code carries MET, and a clause it refuted or downgraded without a code change carries that verdict (REFUTED, DOWNGRADE-NA, DOWNGRADE-MET, DOWNGRADE-DEFENSIBLE, DOWNGRADE-CONDITIONAL, DOWNGRADE-DOCUMENTED) rather than the superseded first-pass GAP; the DOWNGRADE-CRD group (HR-41, HR-42, HR-43, HR-44, GR-34, GR-35) is graded directly as PARTIAL or MET on its case-variant residual instead of carrying that label. See the verification table above for how each was decided.
- shouldmay-<TYPE>.md — verified SHOULD-tier verdicts and MAY catalogue, per type.
