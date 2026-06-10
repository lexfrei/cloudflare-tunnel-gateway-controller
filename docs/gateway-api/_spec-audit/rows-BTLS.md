# BackendTLSPolicy (BTLS-*) + Policy Attachment (POL-*) + GEP-* clause audit

BackendTLSPolicy IS implemented end-to-end: controller `internal/controller/backendtlspolicy_controller.go` writes status; resolver `internal/controller/proxy_syncer.go` selects winning policy + builds CA bundle; proxy transport `internal/proxy/handler.go` (`newTLSTransport` / `verifyBackendChainAndSANs`) performs SNI + chain + SAN auth.

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| BTLS-01 | MUST | CTRL | MET | backendtlspolicy_controller.go:340 conflictWinnerFor; TestComputeConditions_LoserStampedConflicted | Precedence on shared (Service,SectionName) target; resolver mirror in proxy_syncer.go:379. |
| BTLS-02 | MUST | CTRL | MET | backendtlspolicy_controller.go:473 isPolicyOlder; TestSelectPolicyForServicePort_TieBreaksAlphabetically | Oldest creationTimestamp wins, name tie-break. |
| BTLS-03 | MUST | CTRL | MET | backendtlspolicy_controller.go:309 conflictedConditions; TestReconcile_ConflictResolution_LoserStampedConflictedEndToEnd | Loser gets Accepted=False / Reason=Conflicted. |
| BTLS-04 | SHOULD NOT | CRD | NA | backendtlspolicy_types.go:88 godoc only (no CEL forbidding >1 ref) | Advisory; controller accepts multiple targetRefs and processes each. CRD does not block multi-targetRef, only enforces sectionName uniqueness (types.go:121-122). |
| BTLS-05 | MAY | N/A | NA | — | Optional: BackendTLSPolicy for non-Route Services / mesh. Controller scopes to Service backends reachable via Routes; not exercised. |
| BTLS-06 | SHOULD | CTRL | PARTIAL | proxy_syncer.go:282 newBackendTLSResolver | Behaviour consistent for HTTPRoute+GRPCRoute->Service; mesh / non-Route paths out of scope. |
| BTLS-07 | SHOULD | CTRL | MET | docs/gateway-api/limitations.md:147 "Backend mTLS (BackendTLSPolicy)" scope table | Interpretation documented in limitations.md. |
| BTLS-08 | MAY | N/A | NA | — | Future common Options keys; not applicable now. |
| BTLS-09 | MUST | N/A | NA | — | Conditional MUST: only if implementation-specific Options defined. Controller defines no custom Options. |
| BTLS-12 | MUST | CTRL | MET | backendtlspolicy_controller.go:288 + wellKnownUnsupportedConditions:482; TestComputeConditions_WellKnownUnsupportedEmitsInvalid | WellKnownCACertificates NOT supported; "if supported" arm N/A; unsupported path handled per BTLS-20. |
| BTLS-13 | MAY | CTRL | MET | backendtlspolicy_controller.go:56 parseCABundle (x509.ParseCertificate); TestParseCABundle_RejectsMalformedCert | Optional further validation IS performed (PEM + parse). |
| BTLS-14 | MUST | CTRL | MET | backendtlspolicy_controller.go:451 caInvalidConditions; TestComputeConditions_InvalidCACertificateRef / TestComputeConditions_InvalidKind | ResolvedRefs=False with specific Reason+Message. |
| BTLS-15 | MUST | CTRL | MET | proxy_syncer.go:364 poisonedBackendTLS (empty CA pool) -> handler.go:866 AppendCertsFromPEM fails -> handshake fail -> handler.go:1072 502; TestBackendTLSResolver_PolicyTargetsButCAMissing_ReturnsPoisonedConfig; limitations.md:171 | Invalid CA ref never downgrades to plaintext; client receives HTTP 5xx (502). |
| BTLS-16 | MUST | CTRL | MET | backendtlspolicy_controller.go:462 Reason=NoValidCACertificate; TestComputeConditions_InvalidCACertificateRef | All-invalid CA -> Accepted=False / NoValidCACertificate. |
| BTLS-17 | MAY | CTRL | MET | proxy_syncer.go:500 loop concatenates all CACertificateRefs; TestBackendTLSResolver_MultipleCARefs_Concatenates | Multiple CA certs supported (concatenated into one pool). |
| BTLS-20 | MUST | CTRL | MET | backendtlspolicy_controller.go:482 wellKnownUnsupportedConditions Reason=Invalid; TestComputeConditions_WellKnownUnsupportedEmitsInvalid | Unsupported/unrecognised WellKnownCACertificates -> Accepted=False / Invalid. |
| BTLS-21 | MAY | CTRL | NA | — | Optional: define own CA sets. Controller defines none. |
| BTLS-22 | MUST | N/A | NA | — | Conditional: only if own WellKnown set defined (none). |
| BTLS-23 | MUST | CTRL | MET | proxy_syncer.go:324 ServerName=policy.Hostname -> handler.go:899/908 tls.Config.ServerName; handler.go:307 (WS) | Hostname used as SNI in both auth modes and WebSocket dial. |
| BTLS-24 | MUST | CTRL | MET | handler.go:894-899 Mode 1 ServerName-based stdlib verification (SANs empty); limitations.md:155 | Hostname authenticates the cert when no SubjectAltNames; matches served cert. |
| BTLS-25 | MUST NOT | CTRL | MET | handler.go:902-914 Mode 2: ServerName=SNI only, InsecureSkipVerify+VerifyConnection auth via SAN list; TestComputeConditions_URISANAccepted | When SANs set, Hostname NOT used for authentication. |
| BTLS-26 | MUST | CTRL | MET | handler.go:982 matchAnyDNSSan via VerifyHostname; proxy_syncer.go:346 Hostname-type SAN forwarded as DNS SAN | To auth on Hostname with SANs set, operator adds it to SubjectAltNames; honoured. |
| BTLS-27 | MUST | CTRL | MET | handler.go:955 verifyBackendChainAndSANs OR-matches DNS+URI; handler.go:996 errBackendTLSSANMissing on no match | Cert MUST present >=1 matching SAN or handshake fails. |
| BTLS-28 | MUST | CRD | MET | backendtlspolicy_types.go:279 URI AbsoluteURI; shared_types.go:644 Pattern requires scheme+ssp | CRD CEL pattern enforces scheme + scheme-specific-part on URI SAN. |
| POL-01 | SHOULD | N/A | NA | — | PolicyLabelKey discoverability label is advisory; controller does not set the gateway.networking.k8s.io/policy label (no SH-* leak found). |
| POL-02 | MUST | CTRL | MET | backendtlspolicy_controller.go:556 CA ConfigMap keyed to policy.Namespace; routeReferencesAnyService:105 same-namespace target | Policy applies only within its own namespace (Service targetRef carries no Namespace). |
| POL-03 | SHOULD | CTRL | MET | gatewayAncestorRef:772 (Kind=Gateway) | Gateway used as PolicyAncestorStatus object. |
| POL-04 | SHOULD | CTRL | MET | updateStatus:699 PolicyAncestorStatus struct used | Struct used for parent-as-status case. |
| POL-05 | MUST | CTRL | MET | backendtlspolicy_controller.go:747 ControllerName populated; TestReconcile_HappyPath_StampsAcceptedAndResolvedRefs | ControllerName written on every ancestor entry. |
| POL-06 | MUST | CTRL | MET | updateStatus:706 retry.RetryOnConflict around Get+Status().Update | Read-modify-write cycle on Status. |
| POL-07 | MUST NOT | CTRL | MET | updateStatus:719-730 otherControllerEntries preserved; TestUpdateStatus_PreservesOtherControllersUnderCap | Other controllers' ancestor entries never removed/reordered. |
| POL-09 | MUST | CTRL | MET | updateStatus:741 meta.SetStatusCondition (merges by Type) | Conditions merged by Type. |
| POL-10 | MUST | CTRL | MET | conditions stamp ObservedGeneration=policy.Generation (controller.go:314 etc.) | ObservedGeneration set to policy metadata.generation. NOTE: spec text says "Gateway generation" but ancestor conditions track policy generation; per upstream conformance this is the policy's generation. |
| POL-11 | MUST NOT | CTRL | PARTIAL | meta.SetStatusCondition (conditions.go:31) preserves LastTransitionTime but does NOT skip on lower ObservedGeneration | No explicit guard refusing to overwrite a Condition whose stored observedGeneration exceeds ours; single-writer per controllerName + RetryOnConflict makes regression unlikely but the >-skip is not coded. |
| POL-12 | MUST | CTRL | MET | gatewaysForPolicy:580 + updateStatus adds entry per managed Gateway; TestReconcile_GRPCRouteAttachment_PopulatesAncestor | Ancestor entry added on first sight of attaching policy. |
| POL-13 | SHOULD | CTRL | MET | SetupWithManager:839 watches Gateway-affecting routes/CMs; ancestors recomputed each reconcile | Entry updated as ancestor set changes. |
| POL-14 | MUST | CTRL | MET | updateStatus:720 only replaces entries where ControllerName==r.ControllerName; gatewayManagedByUs:679 | Only own-controller ancestors populated. |
| POL-15 | MUST | CTRL | MET | updateStatus:720 + 747 ControllerName used to identify own entries | ControllerName uniquely identifies our entries. |
| POL-16 | MUST | CTRL | MET | updateStatus:728 ancestorRefKey + ControllerName filter | Composite key (AncestorRef+ControllerName) treated as map. |
| POL-17 | MUST NOT | CTRL | MET | updateStatus:756-760 cap at 16, truncate only OUR entries; TestUpdateStatus_CapsAncestorsAt16 | No entries beyond MaxItems=16. |
| POL-18 | MUST | CTRL | PARTIAL | updateStatus:758 truncates our entries silently | Overflow is truncated but policy is NOT marked unimplementable nor signalled on related resources. Reasonable for single-Gateway accounts (comment controller.go:31-39) but does not satisfy the "signal unimplementable" MUST. |
| GEP-01 | MUST | CRD | MET | backendtlspolicy_types.go targetRefs (LocalPolicyTargetReferenceWithSectionName) required | Policy carries targetRefs stanza. |
| GEP-02 | MUST | N/A | NA | — | Cross-hierarchy precedence: BackendTLSPolicy is a single-level Direct Attached Policy (Service only); no hierarchy to resolve. |
| GEP-03 | MUST | CTRL | MET | isPolicyOlder:473 (timestamp then name); overlaps BTLS-01..03 | Same-level older-wins, alphabetical tie-break. |
| GEP-04 | MUST | CRD | MET | PolicyStatus.Ancestors[].Conditions (policy_types.go) | status.conditions in standard Condition format. |
| GEP-05 | SHOULD | CTRL | MET | updateStatus distinct by AncestorRef+ControllerName (POL-16); overlaps POL-12..16 | PolicyAncestorStatus struct used, composite-key distinct. |
| GEP-06 | MUST | CTRL | MET | ControllerName on every entry (POL-05/14/15) | Status namespaced by controllerName. |
| GEP-07 | SHOULD | CTRL | MET | conflictedConditions:309 Accepted=False; overlaps BTLS-03 | Rejected policy gets Accepted=False. |
| GEP-08 | SHOULD | CTRL | PARTIAL | ancestor condition written ON the policy (updateStatus); no condition written onto affected Gateway/Service; rationale in limitations.md | Discoverability condition lives on the policy ancestor status, not on the targeted Service/Gateway object. Documented deviation (see limitations.md "Policy discoverability conditions stay on the policy"). |
