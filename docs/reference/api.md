# API Reference

<!-- Generated from api/v1alpha1 by `make generate`. Do not edit by hand. -->

Field-level reference for the project's own custom resources, generated from the Go types the CRDs are built from. For examples, status conditions and the Gateway API resources the controller watches, see the [CRD Reference](crd-reference.md).

## cf.k8s.lex.la/v1alpha1

Package v1alpha1 contains API Schema definitions for the cf.k8s.lex.la v1alpha1 API group.

### Resource Types

- [ExternalBackend](#externalbackend)
- [ExternalBackendList](#externalbackendlist)
- [GatewayClassConfig](#gatewayclassconfig)
- [GatewayClassConfigList](#gatewayclassconfiglist)
- [GatewayConfig](#gatewayconfig)
- [GatewayConfigList](#gatewayconfiglist)

### ExternalBackend

ExternalBackend is the Schema for the externalbackends API. It is a
namespaced, spec-only resource (no status): a route referencing a missing or
malformed ExternalBackend surfaces the failure on the route's own
ResolvedRefs condition, mirroring how an unresolvable Service backendRef is
reported.

_Appears in:_

- [ExternalBackendList](#externalbackendlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `ExternalBackend` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ExternalBackendSpec](#externalbackendspec)_ |  |  |  |

### ExternalBackendList

ExternalBackendList contains a list of ExternalBackend.

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `ExternalBackendList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ExternalBackend](#externalbackend) array_ |  |  |  |

### ExternalBackendScheme

_Underlying type:_ _string_

ExternalBackendScheme is the wire protocol used to reach an external backend.

_Validation:_

- Enum: [http https]

_Appears in:_

- [ExternalBackendSpec](#externalbackendspec)

| Value | Description |
| --- | --- |
| `http` | ExternalBackendSchemeHTTP reaches the backend over plaintext HTTP.<br /> |
| `https` | ExternalBackendSchemeHTTPS reaches the backend over TLS.<br /> |

### ExternalBackendSpec

ExternalBackendSpec defines an out-of-cluster HTTP(S) endpoint that an
HTTPRoute or GRPCRoute may target as a backendRef. It exists because the
in-process L7 proxy ultimately just dials a URL, so a route can point at an
arbitrary external origin without a Service standing in for it. Unlike a
Service of type ExternalName (which only carries a DNS name and infers the
scheme from the port), this type makes the scheme explicit and lets the host
be an address that is not a valid Service name.

_Appears in:_

- [ExternalBackend](#externalbackend)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `scheme` _[ExternalBackendScheme](#externalbackendscheme)_ | Scheme is the protocol used to dial the backend: "http" or "https". |  | Enum: [http https] <br />Required <br /> |
| `host` _string_ | Host is the backend hostname or IP address (no scheme, port, or path).<br />IPv6 literals must be bracketed (e.g. "[2001:db8::1]"). The controller<br />performs a final URL parse; this pattern only rejects obvious mistakes<br />such as embedding a scheme or path. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-zA-Z0-9._:\[\]-]+$` <br />Required <br /> |
| `port` _integer_ | Port is the backend TCP port. |  | Maximum: 65535 <br />Minimum: 1 <br />Required <br /> |
| `path` _string_ | Path is an optional base path prepended to the request path when the<br />proxy dials the backend. Must begin with "/". It may include a query<br />string (e.g. "/v1?token=abc"); those query parameters are merged into<br />every dialed request, with the request's own parameters taking<br />precedence on a key conflict. |  | Pattern: `^/.*$` <br />Optional <br /> |

### GatewayClassConfig

GatewayClassConfig is the Schema for the gatewayclassconfigs API.
It provides configuration for Cloudflare Tunnel Gateway API implementation.

_Appears in:_

- [GatewayClassConfigList](#gatewayclassconfiglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `GatewayClassConfig` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[GatewayClassConfigSpec](#gatewayclassconfigspec)_ |  |  |  |
| `status` _[GatewayClassConfigStatus](#gatewayclassconfigstatus)_ |  |  |  |

### GatewayClassConfigList

GatewayClassConfigList contains a list of GatewayClassConfig.

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `GatewayClassConfigList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[GatewayClassConfig](#gatewayclassconfig) array_ |  |  |  |

### GatewayClassConfigSpec

GatewayClassConfigSpec defines the desired state of GatewayClassConfig.

_Appears in:_

- [GatewayClassConfig](#gatewayclassconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cloudflareCredentialsSecretRef` _[SecretReference](#secretreference)_ | CloudflareCredentialsSecretRef references a Secret containing Cloudflare API credentials.<br />The Secret must hold a valid Cloudflare API token under the key named by<br />Key, which defaults to "api-token". |  | Required <br /> |
| `accountId` _string_ | AccountID is the Cloudflare account ID. Optional - if not specified, it will be<br />read from the credentials secret ("account-id" key) or auto-detected if the API token<br />has access to only one account.<br />When set, must be a 32-character lowercase hexadecimal string -- the format<br />Cloudflare uses for account IDs. Validated server-side via a CRD-level CEL<br />rule (Kubernetes >= 1.25) so an invalid value is rejected at admission time,<br />before the controller has to reconcile it. Empty string passes through<br />because the field is optional. |  | Optional <br /> |
| `tunnelID` _string_ | TunnelID is the Cloudflare Tunnel UUID. |  | Pattern: `^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$` <br />Required <br /> |
| `allowSharedTunnels` _boolean_ | AllowSharedTunnels permits a Gateway with a dedicated data plane to serve<br />a Cloudflare Tunnel that another namespace's Gateway, or this<br />GatewayClass itself, already serves.<br />Off by default, because holding a tunnel's connector token proves access<br />to the tunnel, not a right to the routes another namespace serves on it,<br />and one token can end up in two namespaces. Sharing a tunnel merges both<br />parties' routes into one ingress document and pushes the union to both<br />parties' proxies. With this off, such a Gateway is refused with<br />Accepted=False/InvalidParameters and none of its routes are programmed.<br />A claim Cloudflare does not confirm is refused either way.<br />Turn it on only where every party on a shared tunnel is trusted to see<br />the others' routes — a single-tenant cluster, or a migration from the<br />shared plane to dedicated ones. It lives here, on the cluster-scoped<br />GatewayClassConfig, precisely so a tenant cannot grant it to themselves. |  | Optional <br /> |
| `maxDataPlanesPerNamespace` _integer_ | MaxDataPlanesPerNamespace limits how many Gateways in one namespace may<br />each have a dedicated data plane.<br />Every opted-in Gateway renders a proxy Deployment, a headless Service, a<br />NetworkPolicy and an optional HPA into its own namespace, and registers a<br />connector on its tunnel. A tenant able to create Gateways and<br />GatewayConfigs can otherwise multiply that as far as they like. Past the<br />cap the newest Gateways are refused with<br />Accepted=False/DataPlaneQuotaExceeded and their planes are not rendered.<br />Oldest first (creation timestamp, then UID), so a Gateway created now<br />takes a free slot or is refused and never evicts one already serving. Two<br />things do evict, tearing down a running plane on the next reconcile:<br />lowering the cap below what a namespace already holds, and an OLDER<br />Gateway opting in later, since the order is by creation timestamp rather<br />than by when the plane was asked for.<br />Counted per namespace over every Gateway with a dedicated data plane,<br />whether it carries spec.infrastructure.parametersRef or gets one from<br />PerGatewayDataPlanes, including ones already refused for<br />something else and ones whose configuration does not currently resolve.<br />Counting only the ones that resolve would let a tenant make a token<br />unreadable to slip another Gateway under the cap.<br />Omitting the field means unlimited, which is what an upgrade that does not<br />set it gets. 0 is rejected rather than accepted as a second spelling of<br />unlimited: it is what an operator writes for "no dedicated planes here",<br />and granting the opposite would fail open. It lives here, on the<br />cluster-scoped GatewayClassConfig, so a tenant cannot raise it. |  | Minimum: 1 <br />Optional <br /> |
| `perGatewayDataPlanes` _[PerGatewayDataPlanes](#pergatewaydataplanes)_ | PerGatewayDataPlanes makes a dedicated data plane the default for every<br />Gateway of this class. Omitted, a Gateway without<br />spec.infrastructure.parametersRef is served by the shared data plane. |  | Optional <br /> |

### GatewayClassConfigStatus

GatewayClassConfigStatus defines the observed state of GatewayClassConfig.

_Appears in:_

- [GatewayClassConfig](#gatewayclassconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array_ | Conditions describe the current state of the GatewayClassConfig. |  | Optional <br /> |

### GatewayConfig

GatewayConfig is the Schema for the gatewayconfigs API: the per-Gateway
data-plane configuration referenced by
Gateway.spec.infrastructure.parametersRef. Its presence opts the Gateway
into hard data-plane isolation — a dedicated proxy Deployment and a
dedicated Cloudflare Tunnel.

_Appears in:_

- [GatewayConfigList](#gatewayconfiglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `GatewayConfig` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[GatewayConfigSpec](#gatewayconfigspec)_ |  |  |  |
| `status` _[GatewayConfigStatus](#gatewayconfigstatus)_ |  |  |  |

### GatewayConfigList

GatewayConfigList contains a list of GatewayConfig.

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `cf.k8s.lex.la/v1alpha1` | | |
| `kind` _string_ | `GatewayConfigList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[GatewayConfig](#gatewayconfig) array_ |  |  |  |

### GatewayConfigSpec

GatewayConfigSpec configures a DEDICATED data plane for one Gateway: its
own proxy Deployment and its own Cloudflare Tunnel, instead of the shared
chart-deployed proxy pool serving every Gateway of the class. Referenced
from Gateway.spec.infrastructure.parametersRef
(group=cf.k8s.lex.la, kind=GatewayConfig, same namespace). A Gateway
without a parametersRef keeps the shared data plane unchanged.

The tunnel identity (tunnel ID and account) is PARSED from the connector
token — there is deliberately no separate tunnelID field, which would
invite token/ID mismatch bugs.

_Appears in:_

- [GatewayConfig](#gatewayconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tunnelTokenSecretRef` _[LocalSecretReference](#localsecretreference)_ | TunnelTokenSecretRef references the Secret (same namespace) holding the<br />Cloudflare Tunnel connector token under the "tunnel-token" key (or Key).<br />The token determines the tunnel this Gateway's data plane serves. |  | Required <br /> |
| `cloudflareCredentialsSecretRef` _[LocalSecretReference](#localsecretreference)_ | CloudflareCredentialsSecretRef optionally overrides the API credentials<br />used to write this Gateway's tunnel ingress document, from a Secret in<br />the SAME namespace under the "api-token" key (or Key). Defaults to the<br />credentials resolved from the Gateway's GatewayClass → GatewayClassConfig<br />(class defaults, Gateway overrides). Namespace-local like every other<br />reference here — a cross-namespace option would let a tenant point the<br />controller at another tenant's credentials. |  | Optional <br /> |
| `authTokenSecretRef` _[LocalSecretReference](#localsecretreference)_ | AuthTokenSecretRef optionally references a Secret (same namespace) with<br />a bearer token (key "auth-token" or Key) protecting this Gateway's proxy<br />config API; the controller authenticates its config pushes with it. |  | Optional <br /> |
| `replicas` _integer_ | Replicas is the fixed proxy replica count. Defaults to 2 (the HA floor<br />for tunnel connectors). Mutually exclusive with autoscaling. Capped at<br />100 — see Autoscaling.MaxReplicas for the rationale. |  | Maximum: 100 <br />Minimum: 1 <br />Optional <br /> |
| `autoscaling` _[ProxyAutoscaling](#proxyautoscaling)_ | Autoscaling renders a HorizontalPodAutoscaler for the proxy Deployment<br />instead of a fixed replica count. |  | Optional <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#resourcerequirements-v1-core)_ | Resources sets the proxy container's resource requests and limits.<br />Defaults to the controller's built-in proxy defaults when unset. |  | Optional <br /> |
| `image` _string_ | Image overrides the proxy container image for this Gateway. Defaults to<br />the controller's --proxy-image flag (set by the Helm chart to the<br />release's proxy image). The pattern is a permissive image-reference sanity<br />check (`registry[:port]/repo[:tag][@digest]`) — it rejects empty, leading<br />junk, and whitespace at admission rather than letting a garbage value fail<br />only at pod-pull time, far from the Gateway's status. |  | MinLength: 1 <br />Pattern: `^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$` <br />Optional <br /> |

### GatewayConfigStatus

GatewayConfigStatus defines the observed state of GatewayConfig. Reserved:
the controller does not write it today (config problems surface on the
REFERENCING Gateway as Accepted=False/InvalidParameters, the actionable
place), and its RBAC deliberately grants no status write. The subresource
exists so adding conditions later is not a breaking CRD change.

_Appears in:_

- [GatewayConfig](#gatewayconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array_ | Conditions describe the current state of the GatewayConfig. |  | Optional <br /> |

### LocalSecretReference

LocalSecretReference references a Secret in the SAME namespace as the
referencing resource — deliberately no Namespace field. A GatewayConfig is
reached through Gateway.spec.infrastructure.parametersRef, which the
Gateway API defines as namespace-local; the credentials it carries follow
the same boundary so one tenant's Gateway cannot point at another tenant's
Secrets.

_Appears in:_

- [GatewayConfigSpec](#gatewayconfigspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Secret. |  | MinLength: 1 <br />Required <br /> |
| `key` _string_ | Key in the Secret. Defaults depend on context: "tunnel-token" for<br />tunnelTokenSecretRef, "auth-token" for authTokenSecretRef, "api-token"<br />for cloudflareCredentialsSecretRef. |  | Optional <br /> |

### PerGatewayDataPlanes

PerGatewayDataPlanes configures the dedicated data plane a Gateway of the
class gets when it names no GatewayConfig itself.

_Appears in:_

- [GatewayClassConfigSpec](#gatewayclassconfigspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `defaultGatewayConfigName` _string_ | DefaultGatewayConfigName gives every Gateway of the class without<br />spec.infrastructure.parametersRef a dedicated data plane, configured as<br />if it referenced the GatewayConfig of this name in the Gateway's own<br />namespace. Its connector token decides the tunnel, so Gateways in one<br />namespace share a tunnel unless one names its own GatewayConfig.<br />A Gateway whose namespace has no GatewayConfig of this name is refused<br />with Accepted=False/InvalidParameters rather than served by the shared<br />data plane. A Gateway's own parametersRef replaces this default; the<br />two are never merged. Tunnel arbitration, AllowSharedTunnels and<br />MaxDataPlanesPerNamespace apply to these Gateways as to any other with<br />a dedicated data plane. |  | MaxLength: 253 <br />MinLength: 1 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` <br />Optional <br /> |

### ProxyAutoscaling

ProxyAutoscaling configures a HorizontalPodAutoscaler for the per-Gateway
proxy Deployment, scaling on the proxy's in-flight request gauge (an
I/O-bound L7 hop saturates on concurrency, not CPU). Serving the metric to
the HPA requires a metrics adapter (prometheus-adapter or KEDA) exposing it
through the custom-metrics API; without one the HPA reports
FailedGetPodsMetric and holds minReplicas.

_Appears in:_

- [GatewayConfigSpec](#gatewayconfigspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minReplicas` _integer_ | MinReplicas is the lower bound. Defaults to 2 — the HA floor for<br />tunnel connectors. |  | Maximum: 100 <br />Minimum: 1 <br />Optional <br /> |
| `maxReplicas` _integer_ | MaxReplicas is the upper bound. Capped at 100: replica counts are<br />tenant-controlled input on a shared cluster, and an unbounded value is<br />a noisy-neighbour attack. Aggregate resource usage still needs a<br />per-namespace ResourceQuota — the cap bounds one Gateway, not a tenant. |  | Maximum: 100 <br />Minimum: 1 <br />Required <br /> |
| `targetInflightPerPod` _integer_ | TargetInflightPerPod is the average in-flight request count per pod the<br />HPA aims for. |  | Minimum: 1 <br />Required <br /> |
| `metricName` _string_ | MetricName overrides the Pods-type custom metric the HPA consumes.<br />Defaults to the proxy's in-flight gauge. |  | Optional <br /> |

### SecretReference

SecretReference is a reference to a Kubernetes Secret.

_Appears in:_

- [GatewayClassConfigSpec](#gatewayclassconfigspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Secret. |  | MinLength: 1 <br />Required <br /> |
| `namespace` _string_ | Namespace of the Secret. Defaults to the namespace the controller runs in. |  | Optional <br /> |
| `key` _string_ | Key in the Secret. Defaults to "api-token". |  | Optional <br /> |
