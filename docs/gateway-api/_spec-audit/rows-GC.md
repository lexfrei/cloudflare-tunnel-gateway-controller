# GatewayClass clause assessment (GC-*)

| ID | Keyword | Class | Status | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| GC-01 | MUST | CTRL | MET | docs/gateway-api/limitations.md:325-327 | Controller propagates GatewayClass changes to existing Gateways; documented as required. |
| GC-02 | SHOULD | CTRL | MET | internal/controller/gatewayclass_controller.go reconcileFinalizer; gatewayclass_finalizer_test.go | gateway-exists-finalizer.gateway.networking.k8s.io is managed: added while any Gateway uses the class, removed when none do; deletion-path covered by tests. |
| GC-03 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:65-75 | Status populated for every GatewayClass whose controllerName matches r.ControllerName. |
| GC-04 | MUST | CRD | NA | api/v1alpha1 (n/a); vendor gatewayclass_types.go:88 | controllerName format is the writer's obligation; CRD has only an immutability CEL rule, no domain-prefixed-path pattern. Controller does string equality, no format check. |
| GC-05 | SHOULD | CTRL | GAP | internal/controller/gatewayclass_controller.go:104-114 | setAcceptedConditions always sets Accepted=True; never validates parametersRef nor sets Accepted=False/InvalidParameters. Resolver (internal/config/resolver.go:91-101) detects bad refs but its errors never reach GatewayClass status. |
| GC-06 | MUST | CRD | NA | api/v1alpha1/gatewayclassconfig_types.go:61 | ParametersReference.Namespace constraint is the writer's obligation. GatewayClassConfig is scope=Cluster so Namespace must be unset; not enforced by controller. |
| GC-07 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:65-67,107-114 | Accepted condition set whenever controllerName matches the controller string. |
| GC-08 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:107-114 | Accepted set to True (controller supports provisioning Gateways for this class). |
| GC-09 | MUST | CTRL | GAP | internal/controller/gatewayclass_controller.go:108-114 | Accepted is hardcoded True; there is no code path that sets it False (e.g. on invalid params or unsupported class). |
| GC-10 | SHOULD | CTRL | NA | internal/controller/gatewayclass_controller.go:104-128 | Message+Reason on Accepted=False is moot because Accepted is never set False (see GC-09). True branch does set Reason=Accepted + Message. |
| GC-11 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:116-127 | SupportedVersion condition is always set alongside Accepted in setAcceptedConditions. |
| GC-12 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:197-217 | SupportedVersion=False (reason UnsupportedVersion) on missing/empty annotation, unparseable version, or major.minor mismatch. Test: TestGatewayClassReconciler_SupportedVersion_MissingAnnotation. |
| GC-13 | MAY | CTRL | NA | internal/controller/gatewayclass_controller.go:156-162 | Optional best-effort path. Controller chooses GC-13 stance: keeps Accepted=True while setting SupportedVersion=False on skew, rather than rejecting. |
| GC-14 | MAY | CTRL | NA | internal/controller/gatewayclass_controller.go:156-162 | Alternative "reject unrecognized CRD versions" path not taken; Accepted stays True. |
| GC-15 | SHOULD | CTRL | MET | internal/controller/gatewayclass_controller.go:212-216 | UnsupportedVersion message names both installed bundle version and the controller's required major.minor. |
| GC-16 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:78-101 | updateStatus uses RetryOnConflict + fresh Get before Status().Update — read-modify-write. |
| GC-17 | MUST NOT | CTRL | MET | internal/controller/gatewayclass_controller.go:107,125 | meta.SetStatusCondition only adds/updates Accepted and SupportedVersion in place; never removes or reorders foreign conditions. |
| GC-19 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:107,125 | meta.SetStatusCondition merges by Type, never duplicates a condition of the same Type. |
| GC-20 | MUST | CTRL | MET | internal/controller/gatewayclass_controller.go:110,151 | ObservedGeneration set to gatewayClass.Generation for both conditions. Test asserts ObservedGeneration==Generation. |
| GC-21 | MUST NOT | CTRL | GAP | internal/controller/gatewayclass_controller.go:107-127 | No guard skipping the update when an existing condition's observedGeneration exceeds the known generation; meta.SetStatusCondition overwrites unconditionally. |
| GC-22 | MUST | CTRL | GAP | internal/controller/gatewayclass_controller.go (absent) | status.supportedFeatures is never populated; sorted-order requirement is vacuously unmet because no features are published at all. |
| GC-23 | SHOULD NOT | NA | NA | vendor gatewayclass_types_overrides.go:52 | supportedFeatureInternal is a vendored backward-compat type; controller code never references it. |
