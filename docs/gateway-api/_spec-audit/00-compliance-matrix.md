# Gateway API v1.5.1 spec compliance matrix

Clause-by-clause audit of the implementation against the normative (RFC-2119) surface of the vendored `sigs.k8s.io/gateway-api v1.5.1` Standard channel. This is the deliverable the closed audit issue asked for: every implemented resource's normative clauses classified honoured / justified-deviation / violated, with code evidence.

## Method

1. Extracted every MUST / MUST NOT / SHOULD / SHOULD NOT / MAY clause from the vendored godoc — 376 rows in `01-clause-inventory.md`, by type.
2. Added cross-cutting GEP/concept requirements not in field godoc (policy attachment GEP-713, route-attachment semantics) — `02-gep-notes.md`.
3. Classified each clause CRD-enforced / controller-actionable / N/A-tunnel and assessed status MET / PARTIAL / GAP / NA against the real code — per-type detail in `rows-<TYPE>.md`.
4. Ran the official conformance suite (Gateway HTTP + gRPC profiles) against a fresh kind cluster + real Cloudflare test tunnel as pass/fail ground truth.
5. Adversarially re-verified every GAP — a skeptic tried to refute each (CRD enforcement, N/A, conditional-satisfied, documented-deviation) before it was allowed to stand. 22 of 25 first-pass GAPs did not survive.

## Dashboard (first-pass classification, 376 clauses)

| Status | Count |
| --- | --- |
| MET | 221 |
| PARTIAL | 31 |
| GAP (first pass) | 25 |
| N/A (tunnel architecture / exempt) | 99 |

Conformance ground truth: 76 top-level subtests PASS, 54 SKIP (documented TLS/TCP/UDP/Mesh/WebSocket/GRPCRouteWeight/HTTPS-listener), **0 FAIL** (`go test ... ok 293s`). The suite is green; the audit's value is the normative surface the suite does not exercise.

## Adversarial verification: 25 first-pass GAPs → final verdicts

| Clauses | First pass | Final verdict | Basis |
| --- | --- | --- | --- |
| HR-41, HR-42, HR-43, HR-44, GR-34, GR-35 | GAP (duplicate match-name first-wins not honoured) | DOWNGRADE-CRD | `Headers`/`QueryParams` are `+listType=map`+`+listMapKey=name` (`vendor/sigs.k8s.io/gateway-api/apis/v1/httproute_types.go:767-783`, `grpcroute_types.go:325`); the API server rejects duplicate names at admission. Header names match case-insensitively, so case-variant names (`Foo`/`foo`) bypass the case-sensitive listMapKey and are ANDed rather than first-wins — a negligible header-only edge (doc note). Query-param names are exact-match per spec, so case-variants are legitimately distinct and there is no residual. |
| SH-47, SH-57 | GAP | **CONFIRMED (MUST NOT)** | route_status.go:112 `Parents = nil` + full `Status().Update` (not SSA) wipes other controllers' RouteParentStatus every reconcile; backendtlspolicy_controller.go:717 preserves foreign entries — route status does not. |
| SH-51, GC-21 | GAP (also GW-81, GW-100, POL-11 same class) | **CONFIRMED (MUST NOT), low** | No observedGeneration regression guard; status writers stamp `ObservedGeneration: generation` unconditionally. Get+RetryOnConflict guards resourceVersion only, not a stale-generation overwrite. Narrow race. |
| HR-04 (and GR-16) | GAP | **CONFIRMED (MUST), minor** | Rule-name uniqueness CEL is experimental-channel only (httproute_types.go:125); shipped Standard CRD strips it; no controller-side uniqueness check. |
| GC-05 | GAP | CONFIRMED but **SHOULD** | Bad parametersRef is surfaced on Gateway status, not as GatewayClass Accepted=False/InvalidParameters. Defensible design deviation; feeds the SHOULD audit. |
| GC-02 | GAP | RESOLVED (was: CONFIRMED but **SHOULD**) | `gateway-exists-finalizer` is now managed by the GatewayClass reconciler (added while any Gateway uses the class, removed when none do). |
| GW-31, GW-87 | GAP | DOWNGRADE-NA | spec.addresses is never user-selectable for a tunnel; same territory as the exempt SupportGatewayStaticAddresses. Doc note only. |
| GW-75, GW-86 | GAP | DOWNGRADE-MET | Precondition is "if empty value NOT supported"; the controller supports empty (claims SupportGatewayAddressEmpty, always auto-assigns the tunnel CNAME), so the obligation is vacuously satisfied. |
| GC-09 | GAP | DOWNGRADE-DEFENSIBLE | Controller reconciles only classes naming its controllerName and supports all of them, so Accepted=True is correct; no "will not support" scenario arises. |
| GC-22 | GAP | DOWNGRADE-CONDITIONAL | Publishing `status.supportedFeatures` is optional; the "MUST be sorted" clause governs order only if published. Not published → vacuously satisfied. |
| SH-36 | GAP | REFUTED | The "Dropped Rule" PartiallyInvalid approach is implemented and tested (route_status.go:334, route_status_diagnostics_test.go:76); the spec requires only one of two approaches. |
| SH-43 | GAP | DOWNGRADE-CONDITIONAL | The per-reconcile full rebuild re-adds only currently-valid own parentRefs, so stale own-entries are dropped naturally; the SHOULD is satisfied for the realistic case. |
| HR-61 | GAP | DOWNGRADE-NA | Redirect `Scheme` enum is http;https; both have well-known ports, so the "scheme without well-known port" precondition is unreachable. |
| GR-44, GR-45 | GAP | DOWNGRADE-NA | A GRPCRoute backend is gRPC-over-HTTP/2 by definition; forcing h2c is correct, and the one protocol-relevant signal (TLS via BackendTLSPolicy) is honoured. |
| OR-03 | GAP | DOWNGRADE-DOCUMENTED | ExternalName Service support is a deliberate, documented deviation (limitations.md:10/32/66) with a stated trust-boundary rationale. Recommend adding an explicit CVE-2021-25740 citation. |

## Confirmed findings (post-verification)

### Code bugs (file as kind/bug)

1. **Route status reconcile clobbers other controllers' `RouteParentStatus` (SH-47, SH-57; MUST NOT).** `internal/controller/route_status.go:112` resets the whole `Parents` slice and writes via full `Status().Update`, so a Route co-managed by another controller loses that controller's parent-status entry every reconcile. Fix: preserve entries whose `ControllerName` differs, mirroring `backendtlspolicy_controller.go:717-762`. Highest severity (multi-controller correctness). Related: listener-status rebuild has the same shape (GW-96/GW-98 PARTIAL).
2. **Status writers lack an observedGeneration regression guard (SH-51, GC-21, GW-81, GW-100, POL-11; MUST NOT).** Status conditions are stamped with the current generation unconditionally; the spec forbids updating a condition whose stored observedGeneration is greater than the writer's known generation. Mitigated by fresh-Get + RetryOnConflict (narrow race), so low severity. Fix: a shared guard, or an accepted-risk note.
3. **HTTPRoute/GRPCRoute rule-name uniqueness not enforced (HR-04, GR-16; MUST).** The uniqueness CEL is experimental-channel; the shipped Standard CRD omits it and the controller does not validate. Minor. Fix: controller-side validation or a documented limitation.

### Documentation additions (justified deviations, recorded in limitations.md by this change)

- spec.addresses is not honoured/validated (tunnel address is not user-selectable; same basis as the exempt static-addresses feature) — recorded under Gateway Listener Configuration.
- ExternalName Service support now cites CVE-2021-25740 in its existing trust-boundary rationale.
- Case-variant duplicate header match names (`Foo` vs `foo`) bypass the case-sensitive CRD listMapKey and are ANDed rather than first-wins — negligible header-only edge (query-param names are exact-match, so unaffected); recorded under Route Conflict Resolution.

## SHOULD / MAY tiers (verified)

The SHOULD and MAY tiers were re-verified in a second pass after the MUST audit — per-type adversarial review for SHOULD, catalogue for MAY. Per-clause detail in `shouldmay-<TYPE>.md`.

### SHOULD / SHOULD NOT (51 clauses)

- HONOURED-TESTED (~22) and N/A for the tunnel architecture (~23) account for the bulk.
- HONOURED-TESTED since the audit (was HONOURED-UNTESTED, 7): HR-21, HR-24, HR-63, BTLS-06, SH-31, SH-32, LS-05 — each now pinned by a regression test (explicit-zero timeouts, redirect Location port, BackendTLS HTTP/gRPC equivalence, reason-vocabulary AST guard, ListenerSet status leak guard).
- DEVIATED-DOCUMENTED (5): GW-74, BTLS-04, GC-05, SH-43, OR-03 — permitted deviations with a written rationale in limitations.md.
- DEVIATED-SILENT (originally 3 distinct gaps across 4 clause IDs) — all resolved since the audit: GC-02 and its v1beta1 alias OTHER-45 are HONOURED (the reconciler now manages the gateway-exists-finalizer); GEP-08 (discoverability condition on the policy ancestor status, not the affected Gateway/Service) and HR-61 (no redirect-port fallback to the listener port — unreachable through the Standard CRD scheme enum http/https) are DEVIATED-DOCUMENTED with rationales in limitations.md. Also resolved earlier: GR-44 / GR-45 (gRPC silently dialing cleartext when a Service declared a TLS appProtocol without a BackendTLSPolicy) now fails the backend closed, matching the HTTP path — #438.

### MAY (34 clauses)

Catalogued implemented / intentionally-omitted; zero worthwhile candidates surfaced. Every MAY is either IMPLEMENTED or OMITTED-INTENTIONAL (edge-terminated TLS, status-only reconciler, single flattened ingress). The optional surface is a deliberate product choice.

## Provenance

- `01-clause-inventory.md` — verbatim clause extraction (376 rows).
- `02-gep-notes.md` — GEP/concept cross-cutting requirements.
- `rows-<TYPE>.md` — first-pass per-clause classification + evidence (GW, HR, GR, SH, GC, RG, BTLS, LS, OTHER). For the 25 first-pass GAPs, the verdicts in the verification table above supersede the per-row status.
- shouldmay-<TYPE>.md — verified SHOULD-tier verdicts and MAY catalogue, per type.
