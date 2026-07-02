# GEP / concept-level requirements (Phase 2)

Cross-cutting normative requirements from `gateway-api.sigs.k8s.io` that are NOT expressed in field-level godoc (those are in `01-clause-inventory.md`). Sourced from GEP-713 (Policy Attachment) and the route-attachment / status-condition concept pages, originally captured at v1.5 and refreshed for v1.6.0 (see `00-compliance-matrix.md`, "v1.5.1 → v1.6.0 refresh"). The HTTPRoute precedence narrative duplicates HR-05..08 and is not re-listed.

ID prefix: GEP-NN.

## Policy attachment & conflict resolution (GEP-713)

Scope note: this controller implements no general policy-attachment CRDs. The only upstream policy type in scope is **BackendTLSPolicy** (a Direct Attached Policy); `GatewayClassConfig` is the controller's own CRD, not a Gateway API policy. GEP-713 requirements therefore apply to BackendTLSPolicy handling (and overlap the BTLS / POL godoc clauses).

| ID | Topic | Keyword | Requirement |
| --- | --- | --- | --- |
| GEP-01 | targetRefs | MUST | Every policy MUST include a `targetRefs` stanza specifying which resource(s) the policy augments (or a singular `targetRef` MAY be used for single-target policies). |
| GEP-02 | conflict: hierarchy | MUST | Between two policies at different hierarchy levels, the one attached higher (less specific) MUST be the established one. |
| GEP-03 | conflict: same level | MUST | Between two policies at the same level, the older by creation timestamp MUST be the established one; ties broken by first alphabetical `{namespace}/{name}`. (Overlaps BTLS-01..03.) |
| GEP-04 | status conditions | MUST | Policy CRDs MUST define a `status` stanza containing a `conditions` stanza in the standard Condition format. |
| GEP-05 | PolicyAncestorStatus | SHOULD | Policy objects SHOULD use the upstream `PolicyAncestorStatus` struct; entries MUST be distinct by `AncestorRef` + `ControllerName`. (Overlaps POL-12..16.) |
| GEP-06 | status namespacing | MUST | Any status added to policy objects or targets MUST be namespaced by the implementation (controllerName). (Overlaps POL-14/15.) |
| GEP-07 | rejected policy | SHOULD | A policy rejected under the None merge strategy SHOULD have its `Accepted` condition set to false. (Overlaps BTLS-03.) |
| GEP-08 | discoverability | SHOULD | To support policy discoverability, implementations SHOULD put a condition into `status.Conditions` of any objects affected by the policy. |

## Route attachment & status semantics (concept pages)

| ID | Topic | Keyword | Requirement |
| --- | --- | --- | --- |
| GEP-10 | Accepted meaning | (def) | A resource is Accepted for processing when its attachment succeeds enough to generate some configuration. (Frames SH-45: "Accepted if at least one rule is implemented".) |
| GEP-11 | sectionName miss | MUST | If there are no relevant Listeners (e.g. a sectionName is specified that does not exist on the parent), the Route has nowhere to attach and MUST have Accepted set to false for that parentRef. (Overlaps SH-08/09/14/15.) |
| GEP-12 | ResolvedRefs message | SHOULD | When ResolvedRefs is false, the message field SHOULD indicate which reference is invalid. |
| GEP-13 | per-parentRef status | (semantics) | Route status is per-parentRef and independent: a Route can be Accepted by one parentRef and not another. (Frames SH-54/56/58.) |
| GEP-14 | match combination | (restate) | Within a single match block, match types are ANDed; multiple match blocks in a rule are ORed. (Restates HR/GR match godoc.) |
| GEP-15 | route merging | (restate) | More than one Route can bind to a Gateway; Routes merge on a Gateway as long as they do not conflict. (Frames HR-05/06 cross-route precedence.) |

## Well-known labels for generated resources (GEP-1762, v1.6.0)

| ID | Topic | Keyword | Requirement |
| --- | --- | --- | --- |
| GEP-16 | `gateway.networking.k8s.io/gateway-name` / `gateway-class-name` | should/must (lc, non-normative) | New in v1.6.0 (kubernetes-sigs/gateway-api#4705): `apis/v1/well_known_labels.go` defines well-known label keys an implementation MAY stamp on resources it generates on behalf of a Gateway/GatewayClass, so consumers can identify the owning object. The godoc keywords are lowercase and carry the RFC-8174 non-normative caveat, so this is informational rather than an audited MUST/SHOULD row. |

The per-Gateway rendered data plane (`internal/render/render.go`) stamps its own selector label `cf.k8s.lex.la/gateway`, not the well-known keys. Adopting the well-known label as an additional stamp is a candidate improvement, not a compliance gap.

## Support-level framing (for classifying SHOULD/MAY)

Gateway API tiers features as Core (mandatory for the resource), Extended (optional; if claimed, MUST pass conformance), and Implementation-specific. The controller's claimed tier per feature lives locally in `test/conformance/conformance_test.go` (`SupportedFeatures` / `ExemptFeatures` / `SkipTests`). Phase 3 classification uses that as the authority for which "if supported" MUSTs are in force, and Phase 4 overlays the real conformance run (background ID bi6d8o2g3) as pass/fail ground truth.

Notable godoc-encoded tier facts:

- GRPCRoute is Extended support (GR-01): its MUSTs apply because the controller claims `SupportGRPCRoute`.
- Listener Isolation is Extended and explicitly NOT claimed (exempt) — GW-26/27 require documenting that and not claiming the feature.
- BackendTLSPolicy / ListenerSet are claimed in `SupportedFeatures`, so their MUSTs are in force.
