# SHOULD / MAY audit — ListenerSet (LS-), object_reference (OR-), exempt types (OTHER-)

Gateway API v1.5.1 (Standard channel, vendored). Architecture: in-process L7 proxy; TLS terminated at Cloudflare edge (no in-cluster SNI layer); TLSRoute/TCPRoute/UDPRoute exempt. Conformance suite green this cycle including `features.SupportListenerSet`.

## SHOULD / SHOULD NOT table

| ID | Keyword | Verdict | Evidence | Notes |
| --- | --- | --- | --- | --- |
| LS-02 | SHOULD (lc) | N-A | No Policy CRD targets ListenerSet listeners; grep PolicyTargetRef/attachPolicy = only BackendTLSPolicy which targets Services not listeners | "Reject policy if it cannot apply to specific listeners" — controller implements no Policy that attaches to ListenerSet listeners, so the clause has nothing to act on. |
| LS-05 | SHOULD | HONOURED-TESTED (TestListenerSetStatus_NeverLeaksSecretContents) | listenerset_controller.go:204-271 computeAcceptance (messages = own fixed constants + operational err.Error() only); buildListenerSetEntryStatuses:577-612 (per-entry verdicts only); listenerSetMessageForAccepted/Programmed:545-571 | Status never echoes parent/sibling Secret contents or sibling TLS cert data; only the child's own-entry verdicts plus operational error strings (list/eval failures) surface. Honoured by construction. No dedicated test asserts non-leak. |
| LS-10 | SHOULD | N-A | listenerset_types.go:143 (HTTPS SHOULD match SNI+Host); docs/gateway-api/limitations.md:101 (edge TLS termination); same UnsupportedProtocol verdict applies to LS entries | HTTPS dual-layer SNI+Host match: TLS terminated at Cloudflare edge, no in-cluster SNI layer to match; HTTP-layer Host still matched by proxy. Documented. |
| LS-40 | SHOULD NOT (lc) | HONOURED-TESTED | listenerset_controller.go only emits ListenerConditionAccepted/Programmed/ResolvedRefs/Conflicted (lines 655,663,691,822,830,838,877,885); grep ListenerEntryConditionReady outside vendor = none | Reserved "Ready" condition is never set by the controller; conformance (SupportListenerSet) green. |
| OR-03 | SHOULD NOT | DEVIATED-DOCUMENTED | service_resolver.go:199-200 actively supports ExternalName (scheme://ExternalName:port); object_reference_types.go:116 (SHOULD NOT, CVE-2021-25740); docs/gateway-api/limitations.md:10,32 | Deliberate documented deviation: ExternalName Services are supported; limitations.md:32 cites CVE-2021-25740 and the namespace-write + ReferenceGrant trust boundary. Per task brief — DO NOT re-file. |
| OTHER-45 | SHOULD | HONOURED-TESTED | gatewayclass_controller.go reconcileFinalizer; v1beta1/gatewayclass_types.go:46 | v1beta1 alias of GC-02 (same v1 storage, same reconciler). Resolved together with GC-02: the finalizer is now managed; see the GC-02 row. |

## MAY table

| ID | Verdict | Evidence | Notes |
| --- | --- | --- | --- |
| LS-04 | OMITTED-INTENTIONAL | listenerset_types.go:98 (MAY reject via TooManyListeners); grep TooManyListeners in internal/ = none; CRD MaxItems=64 caps entries at API-server level | Controller never emits the TooManyListeners ListenerEntryStatus reason. The MAY is optional; the apiserver CEL MaxItems=64 already bounds entry count, so the runtime rejection condition is intentionally not implemented. |
| LS-14 | IMPLEMENTED-TESTED | routebinding/listenerset.go:101 ValidateBindingForListenerSet -> evaluateListenerBinding (hostname + namespace + kind + sectionName/port); buildListenerSetEntryStatuses:589 FilterSupportedKinds; tests internal/routebinding/listenerset_test.go; conformance features.SupportListenerSet | AllowedRoutes namespace (Same/All/Selector) + kind filtering honoured per ListenerSet entry. Covered by unit tests and the green conformance ListenerSet feature. |
