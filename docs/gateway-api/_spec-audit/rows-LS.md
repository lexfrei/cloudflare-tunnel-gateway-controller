# ListenerSet clause assessment (LS-*)

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| LS-01 | MUST (lc) | CTRL | MET | internal/routebinding/listenerset.go:20 EvaluateListenerSetAcceptance; getListenerNamespaceFrom defaults to None | Parent must opt in via allowedListeners; default From=None rejects (NotAllowed). |
| LS-02 | SHOULD (lc) | N/A | NA | — | Policy-attachment to specific listeners; controller implements no Policy CRDs targeting ListenerSet listeners. |
| LS-03 | MUST | CTRL | MET | internal/listenermerge/merge.go:96 Merge + sortListenerSets:164; TestMerge_PrecedenceOrdering | Merged view: Gateway first, then ListenerSet by creationTimestamp, then namespace/name. |
| LS-04 | MAY | CRD | MET | listenerset_types.go:112 MaxItems=64; not surfaced as TooManyListeners condition | MAY clause; CRD caps entries at 64. Controller never emits TooManyListeners (allowed, it is optional). |
| LS-05 | SHOULD | CTRL | MET | internal/controller/listenerset_controller.go:104-107 (no sibling/parent secret data leaked into status) | Status reports only own-entry verdicts; no parent/sibling secret contents surfaced. |
| LS-06 | MUST | CRD | MET | listenerset_types.go:117 XValidation "Listener name must be unique"; +listMapKey=name:110 | API-server CEL enforces name uniqueness within a ListenerSet. |
| LS-07 | MUST | CTRL | PARTIAL | internal/routebinding/hostname.go:14 HostnamesIntersect; docs/gateway-api/limitations.md:101 | HTTP Host matching done in proxy; TLS/HTTPS SNI N/A (edge terminates TLS). Only HTTP layer applies. |
| LS-08 | MUST | N/A | NA | docs/gateway-api/limitations.md:101,203 | TLS SNI match: TLS terminated at Cloudflare edge; no in-cluster SNI layer. |
| LS-09 | MUST | CTRL | MET | internal/routebinding/hostname.go:35 hostnameMatches; internal/proxy/router.go host routing | Listener Hostname matched against Host header by the in-process proxy. |
| LS-10 | SHOULD | N/A | NA | docs/gateway-api/limitations.md:101,165 | HTTPS dual-layer SNI+Host: edge-terminated TLS means no SNI layer; HTTP layer still matched. |
| LS-11 | MUST | CTRL | MET | docs/gateway-api/limitations.md:101,203 (edge TLS termination documented) | Implementation does not ensure SNI match; documented per the MUST-document escape clause. |
| LS-12 | MUST | CTRL | MET | internal/routebinding/binding.go:169 (HostnamesIntersect gates binding); internal/routebinding/hostname.go:14 | Route accepted only when listener and route hostnames intersect (NoMatchingListenerHostname otherwise). |
| LS-13 | MUST | N/A | NA | docs/gateway-api/limitations.md:165,203 | Longest-SNI cert selection: no in-cluster TLS handshake; edge terminates TLS. |
| LS-14 | MAY | CTRL | MET | internal/routebinding/listenerset.go:101 ValidateBindingForListenerSet via evaluateListenerBinding (namespace+kind) | AllowedRoutes namespaces+kinds honoured per ListenerSet entry. |
| LS-15 | MUST | CTRL | MET | internal/proxy/converter.go:99 sortRoutesByPrecedence + internal/proxy/router.go:510 computePriority/sortRulesByPrecedence | Most-specific by match type (priority score), then oldest creationTimestamp, then namespace/name. |
| LS-19 | MUST | CTRL | MET | internal/controller/listenerset_controller.go:527,535 uses ListenerSetConditionAccepted/Programmed + ListenerSetReason* constants | Top-level conditions stamped with the spec's ListenerSet condition/reason constants. |
| LS-20 | MUST | CTRL | MET | internal/routebinding/kind.go:108 FilterSupportedKinds; listenerset_controller.go:589 | SupportedKinds reflects implementation-supported kinds (HTTPRoute/GRPCRoute) for the entry. |
| LS-21 | MUST NOT | CTRL | MET | internal/routebinding/kind.go:130-137 (only supported kinds appended) | Unsupported specified kinds excluded from SupportedKinds list. |
| LS-22 | MUST | CTRL | MET | internal/controller/listenerset_controller.go:896 listenerEntryResolvedRefsCondition → InvalidRouteKinds | Entry ResolvedRefs=False / InvalidRouteKinds when no supported kind or invalid kind present. |
| LS-23 | MUST | CTRL | MET | internal/routebinding/kind.go:130 (valid kinds still appended when mixed) + listenerset_controller.go:911 hasInvalidKind branch | Mixed valid+invalid: valid kinds still listed in SupportedKinds. |
| LS-24 | MUST | CTRL | MET | internal/controller/listenerset_controller.go:290 countAttachedRoutesPerEntry; comment:276-289; TestListenerSetReconciler_ConflictedEntryStillCountsAttachedRoutes | AttachedRoutes set even when entry Accepted=False (conflicted/unresolved-ref). |
| LS-25 | MUST NOT | CTRL | MET | internal/controller/listenerset_controller.go:394 ValidateBindingForListenerSet gates count on route Accepted only; comment:390-393 | Only routes with binding-Accepted=True counted; others excluded. |
| LS-35 | MUST (lc) | CTRL | MET | internal/controller/listenerset_tls.go:119,143 validateListenerSetSecretExists (InvalidCertificateRef only after allow check); TestResolveListenerEntryRefs_SecretNotFound | InvalidCertificateRef used only for allowed refs (same-ns or grant-permitted). |
| LS-36 | MUST (lc) | CTRL | MET | internal/controller/listenerset_tls.go:100-117 (cross-ns w/o grant → RefNotPermitted); TestResolveListenerEntryRefs_CrossNamespace_NoGrant | Disallowed cross-namespace ref → RefNotPermitted, not InvalidCertificateRef. |
| LS-40 | SHOULD NOT (lc) | CTRL | MET | internal/controller/listenerset_controller.go (no ListenerEntryConditionReady emitted; only Accepted/Programmed/Conflicted/ResolvedRefs) | Reserved "Ready" condition is never set by the controller. |
