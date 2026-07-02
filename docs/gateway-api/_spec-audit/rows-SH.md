# SH (shared_types) clause audit

Audited against vendored `sigs.k8s.io/gateway-api v1.6.0` (originally at v1.5.1, refreshed per `00-compliance-matrix.md` "v1.5.1 → v1.6.0 refresh"; no delta lands on a shared-type row here — the cross-type HTTPRoute/GRPCRoute hostname MUST→MAY relaxation, kubernetes-sigs/gateway-api#4598, is audited under GR-14/GR-15 in `rows-GR.md`, not SH). Class: CRD = enforced by kubebuilder/CEL markers; CTRL = controller logic; N/A = not applicable (reason in Notes). GEP-10..15 (route-attachment / per-parentRef status semantics) factored in for SH-08/09/14/15/44/45/54/56/58.

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| SH-06 | MAY | CTRL | NA | routebinding/binding.go:34 | Optional: attaching to non-Gateway parents not supported; only Gateway/ListenerSet handled. |
| SH-07 | MUST | CTRL | NA | docs/gateway-api/limitations.md | Conditional on SH-06 (other parent kinds); none supported, so no SectionName-interpretation doc owed. |
| SH-08 | MUST | CTRL | MET | routebinding/binding.go:62-81 (makeBindingResult) | 1-of-N listeners accept -> matched non-empty -> Accepted=True. |
| SH-09 | MUST | CTRL | MET | routebinding/binding.go:66-72; route_status.go:353 | No listener accepts -> Accepted=False (detached) for that parentRef. |
| SH-12 | MAY | CTRL | NA | routebinding/binding.go | Optional: only Gateway/ListenerSet parents; no other parent resources. |
| SH-13 | MUST | CTRL | NA | — | Conditional on SH-12; no other parent kinds, so no Port-interpretation doc owed. |
| SH-14 | MUST | CTRL | MET | routebinding/binding.go:105-141 (findMatchingEntries) | Port-filtered match; 1-of-N accept -> Accepted=True. |
| SH-15 | MUST | CTRL | MET | routebinding/binding.go:128-138 | Port-filtered, none accept -> rejection reason -> Accepted=False. |
| SH-16 | MUST NOT | CTRL | NA | grep GatewayDefaultScope -> none | Default-gateway claiming (experimental) not implemented; controller never claims default Gateways. |
| SH-17 | MUST NOT | CTRL | NA | grep GatewayDefaultScope -> none | Same: GatewayDefaultScopeNone unimplemented; vacuously satisfied. |
| SH-18 | MUST | CRD | MET | vendor shared_types.go:177 CommonRouteSpec | Type embedded in HTTPRoute/GRPCRoute specs; CRD-enforced. |
| SH-19 | must (lc) | CRD | NA | shared_types.go:199 (prose, no CEL) | Non-normative prose; not enforced by any CEL marker in vendored CRD; controller does not de-dupe. |
| SH-26 | SHOULD | CTRL | MET | converter.go:1303-1346 (resolveBackendProtocol) | Honors Service port appProtocol (h2c/ws/wss/https). |
| SH-27 | SHOULD | CTRL | MET | converter.go:44-58 (kubernetes.io/h2c,ws,wss) | Recognizes KEP-3726 standard app protocols. |
| SH-28 | MAY | CTRL | MET | converter.go:1333 (fallback HTTP/1.1) | Unset/unknown appProtocol -> infers HTTP/1.1 default. |
| SH-30 | MUST | CTRL | MET | converter.go:1337-1338,1365-1366; route_status.go:405-413 | TLS appProtocol w/o BackendTLSPolicy & unknown appProtocol -> ResolvedRefs=False, Reason=UnsupportedProtocol. |
| SH-31 | should (lc) | CTRL | MET | route_status.go:345-364 (only std RouteReason* used) | Accepted uses only standard reasons (Accepted/Pending/NoMatchingParent/...). |
| SH-32 | should (lc) | CTRL | MET | route_status.go:382-413 (std reasons) | ResolvedRefs uses only standard reasons (ResolvedRefs/RefNotPermitted/InvalidKind/BackendNotFound/UnsupportedProtocol). |
| SH-33 | MUST | CTRL | MET | route_status.go:251-294 (diagnosticConditions) | PartiallyInvalid path chooses drop-rule approach; Fall Back (last-known-good) not used. |
| SH-34 | MUST | CTRL | MET | route_status.go:334 ("Dropped Rule " prefix) | PartiallyInvalid message starts "Dropped Rule" + names rule indices. |
| SH-35 | MUST | CTRL | MET | route_status.go:227-228,287-289 | PartiallyInvalid only added when Accepted=True; ObservedGeneration=latest generation. |
| SH-36 | MUST | CTRL | GAP | route_status.go:330-334 | "Fall Back" (last-known-good) approach not implemented; only "Dropped Rule" path exists. Conditional MUST — applies only if that approach is chosen, so non-blocking, but no Fall Back prefix exists. |
| SH-37 | MUST | CTRL | NA | route_status.go:251-294 | Conditional on SH-36 (Fall Back approach); not chosen -> no last-known-good generation to set. |
| SH-38 | should (lc) | CTRL | NA | — | Non-normative; Fall Back requires restart-state restoration which the controller does not do (drop-rule chosen instead). |
| SH-39 | MUST NOT | CTRL | MET | route_status.go:227,278-293 | PartiallyInvalid set only when some-but-not-all rules dropped AND Accepted=True; never on fully valid/invalid/unaccepted. |
| SH-40 | MUST | CTRL | MET | route_status.go:286-293,227 (only appended when True) | Condition only ever appended with Status=True; never set to False. |
| SH-42 | MUST | CTRL | MET | route_status.go:240 (ControllerName: ...) | Every RouteParentStatus populated with this controller's name. |
| SH-43 | should (lc) | CTRL | GAP | grep cleanup/stale -> none in route path | No cleanup of stale own-controllerName parent entries; blanket Parents=nil rebuild only re-adds currently-managed refs, so stale own entries for refs removed from spec ARE dropped (incidentally satisfies the spirit), but no finalizer-based cleanup when route still references a now-unmanaged parent. |
| SH-44 | MUST | CTRL | MET | route_parent_binding.go; route_status.go:149-167 | Accepted condition set for refs to managed Gateways supporting the route kind (kind check in routebinding/kind.go). |
| SH-45 | MUST | CTRL | MET | route_status.go:278 (len(wholeRuleIdx) >= ruleCount) | Accepted=False only when EVERY rule wholly unservable; >=1 servable rule -> Accepted=True. |
| SH-46 | MUST | CTRL | MET | route_status.go:86,106,127 (RetryOnConflict + Get + Update) | Read-modify-write: fetch fresh route, mutate, Status().Update under conflict retry. |
| SH-47 | MUST NOT | CTRL | GAP | route_status.go:112 (routeStatus.Parents = nil) | Blanket reset of Parents then rebuild only own managed entries -> a foreign controller's RouteParentStatus entry on a route also attached to our Gateway is REMOVED. cf. backendtlspolicy_controller.go:35 which preserves foreign entries. Multi-controller-per-route violation. |
| SH-49 | MUST | CTRL | PARTIAL | route_status.go:219-229 (conditions slice rebuilt) | Conditions are rebuilt by Type each reconcile (same Type merged by full replacement, not stacked) so dup-Type cannot occur; but done via slice rebuild not meta.SetStatusCondition, and LastTransitionTime is always set to now (not preserved across no-op transitions). |
| SH-50 | MUST | CTRL | MET | route_status.go:287-289,369,420 (ObservedGeneration: generation) | Every condition gets observedGeneration = route metadata.generation at update time. |
| SH-51 | MUST NOT | CTRL | GAP | route_status.go (no FindStatusCondition compare) | No stale-generation guard: writer unconditionally sets observedGeneration without checking whether the existing condition's observedGeneration is already greater. |
| SH-53 | MUST | CRD | MET | vendor shared_types.go:554 RouteStatus | Embedded in HTTPRoute/GRPCRoute status; CRD-enforced. |
| SH-54 | MUST | CTRL | MET | route_status.go:231-242 (ParentRef+ControllerName per entry) | Each entry keyed by parentRef + controllerName. |
| SH-56 | MUST | CTRL | PARTIAL | route_status.go:118-125,158-167 | Only writes entries for refs selecting managed Gateways (own controllerName written), but achieved via full-slice reset rather than selective per-entry update — see SH-47. |
| SH-57 | MUST NOT | CTRL | GAP | route_status.go:112 | Same root cause as SH-47: Parents=nil reset can discard a non-matching-controllerName entry when the route is also attached to our Gateway. |
| SH-58 | MUST | CTRL | MET | route_status.go:118-125 (loop per parentRef); GEP-13 | Each parentRef evaluated independently (per-ref bindingResults), status per-ref. |
| SH-59 | MUST | CTRL | MET | route_status.go:86,106,127 | Read-modify-write on Parents (same path as SH-46). |
| SH-63 | MUST NOT | CRD | MET | vendor shared_types.go:644 (Pattern enforces full URI) | AbsoluteURI Pattern regex rejects relative URIs (requires scheme group). |
| SH-64 | MUST | CRD | MET | vendor shared_types.go:644 (RFC3986 Pattern) | AbsoluteURI kubebuilder Pattern is the RFC3986 regex. |
| SH-65 | MUST | CRD | MET | vendor shared_types.go:644 (scheme group required) | Pattern requires (scheme:) + scheme-specific-part. |
| SH-66 | MUST | CRD | PARTIAL | vendor shared_types.go:644 | Pattern requires authority shape but does not deeply validate FQDN/IP host; CRD-level best-effort. |
| SH-67 | MUST NOT | CRD | MET | vendor shared_types.go:653 (CORSOrigin Pattern) | CORSOrigin Pattern requires http(s):// or single '*'; relative rejected. |
| SH-68 | MUST | CRD | MET | vendor shared_types.go:653 | CORSOrigin Pattern is the RFC3986-derived regex. |
| SH-69 | MUST | CRD | MET | vendor shared_types.go:653 | Pattern allows scheme+specific-part OR single '*'. |
| SH-71 | MUST | CRD | PARTIAL | vendor shared_types.go:653 | Authority/host shape enforced by Pattern but no deep FQDN/IP validation. |
| SH-77 | should (lc) | N/A | NA | grep SessionPersistence -> none | SessionPersistence unimplemented (no controller/proxy surface); clause is user-facing guidance, non-normative. |
