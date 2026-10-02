# Security

This document covers the security policy and best practices for the Cloudflare Tunnel Gateway Controller.

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest 3.x minor | Yes |
| Earlier 3.x minors | No |
| < 3.0 | No |

Fixes land on the latest 3.x minor; older minors are not backported.

## Reporting Vulnerabilities

!!! danger "Do Not Use Public Issues"

    Please do not report security vulnerabilities through public GitHub issues.

Report vulnerabilities via email:

- **Email**: <f@lex.la>
- **GPG Key**: `F57F 85FC 7975 F22B BC3F 2504 9C17 3EB1 B531 AA1F`

### What to Include

- Type of vulnerability
- Full paths of affected source files
- Location of affected source code (tag/branch/commit)
- Step-by-step reproduction instructions
- Proof-of-concept or exploit code (if possible)
- Impact assessment

### Response Timeline

| Stage | Timeline |
|-------|----------|
| Initial Response | Within 48 hours |
| Status Update | Within 7 days |
| Fix Timeline | Depends on severity |

## Security Best Practices

### API Token Management

The Cloudflare API token is sensitive and should be:

1. **Stored in Kubernetes Secret**

    ```bash
    kubectl create secret generic cloudflare-credentials \
      --from-literal=api-token="${CF_API_TOKEN}"
    ```

2. **Scoped with minimum permissions**
   - Account: Cloudflare Tunnel (Edit, Read)

3. **Rotated regularly**
   - Create new token in Cloudflare dashboard
   - Update Kubernetes secret
   - Controller picks up new token on restart

4. **Never committed to git**
   - Use external secret management (Vault, AWS Secrets Manager)

### RBAC Configuration

The controller requires specific Kubernetes permissions:

```yaml
# Minimum required permissions -- matches charts/.../templates/clusterrole.yaml
rules:
  # Gateway API - read specs
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["httproutes", "grpcroutes", "referencegrants", "backendtlspolicies", "listenersets"]
    verbs: ["get", "list", "watch"]
  # GatewayClasses - the controller manages the spec-defined gateway-exists
  # finalizer (metadata write outside the status subresource)
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gatewayclasses"]
    verbs: ["get", "list", "watch", "update", "patch"]
  # Gateways - status patches require update/patch on the parent resource
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways"]
    verbs: ["get", "list", "watch", "update", "patch"]
  # Gateway API status subresources - write status
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gatewayclasses/status", "gateways/status", "httproutes/status", "grpcroutes/status", "backendtlspolicies/status", "listenersets/status"]
    verbs: ["get", "update", "patch"]
  # ServiceImport - a backendRef may target an imported multicluster Service
  - apiGroups: ["multicluster.x-k8s.io"]
    resources: ["serviceimports"]
    verbs: ["get", "list", "watch"]
  # CustomResourceDefinitions - Get of the gatewayclasses CRD to read the
  # bundle-version annotation for the SupportedVersion condition, and of the
  # controller's own CRDs at startup to report fields their schema lacks
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get"]

  # Core API
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get", "list", "watch"]
  # Services - read everywhere (backend resolution) plus full write for the
  # per-Gateway data planes: the controller renders a headless config Service
  # per opted-in Gateway. Rendered objects are controller-owned via
  # ownerReferences and deleted only when owned.
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  # Secrets - read for credentials; create for the generated config-API auth Secret, both the per-Gateway one and the shared plane's, and for the config API TLS CA and serving certificates (no update/delete here: the token is never rotated, and the shared plane's certificate gets update from a namespaced Role on that one Secret).
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch", "create"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch"]
  # EndpointSlice - the proxy endpoint reconciler discovers proxy pods so a
  # newly-joined replica gets the cached config pushed immediately
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
  # Events - route reconcilers emit Events via both the core (v1) and the new
  # (events.k8s.io/v1) recorders; grant both so neither path is denied
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  - apiGroups: ["events.k8s.io"]
    resources: ["events"]
    verbs: ["create", "patch"]

  # Deployments - the proxy Secret reconciler records the tunnel-token
  # revision on the proxy Deployment's metadata and patches its pod-template
  # annotation to roll pods when that Secret rotates, and the per-Gateway data planes render a dedicated proxy Deployment per
  # opted-in Gateway (full write, cluster-wide, because Gateways live in
  # arbitrary namespaces)
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # HorizontalPodAutoscalers - rendered per opted-in Gateway when its
  # GatewayConfig requests autoscaling
  - apiGroups: ["autoscaling"]
    resources: ["horizontalpodautoscalers"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  # NetworkPolicies - rendered per opted-in Gateway to lock the proxy's
  # config-API port to the controller (+ monitoring) namespaces
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]

  # GatewayConfig CRD - per-Gateway data-plane parameters referenced from
  # Gateway.spec.infrastructure.parametersRef
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["gatewayconfigs"]
    verbs: ["get", "list", "watch"]

  # GatewayClassConfig CRD
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["gatewayclassconfigs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["gatewayclassconfigs/status"]
    verbs: ["get", "update", "patch"]
  # ExternalBackend CRD - a backendRef may target an out-of-cluster endpoint
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["externalbackends"]
    verbs: ["get", "list", "watch"]

  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

!!! note "RBAC scope"
    The controller reads Secrets and ConfigMaps and writes status subresources. Its workload writes are scoped to the data planes it owns. On the shared proxy Deployment it records the tunnel-token revision in an annotation, and patches its pod-template annotation when that Secret rotates (a native rolling restart). For each Gateway opted into a per-Gateway data plane via `infrastructure.parametersRef` it renders a dedicated proxy Deployment, headless config Service, and optional HorizontalPodAutoscaler. Those rendered objects are controller-owned via ownerReferences, kept in sync against drift, and deleted only when actually owned — a name collision with a user resource can never turn into a deletion. Workload write access is cluster-wide because Gateways live in arbitrary namespaces.

    Because the RBAC grant for these resources is broad (`delete` on Deployments, Services, and HorizontalPodAutoscalers cluster-wide), the **in-code ownership check is the security boundary, not the RBAC scope**. Every apply and create path for a *per-Gateway* rendered object — including that plane's generated config-API auth Secret — refuses to adopt, update, or GC an object at a rendered name unless it already carries this Gateway's controller ownerReference. A pre-existing object with a foreign owner (or none) is left untouched and the reconcile surfaces a `RenderFailed` event instead of overwriting it. The *shared* plane's generated auth Secret (below) has no Gateway to check ownership against, so it has no equivalent adoption check: it reuses whatever Secret already exists at its deterministic name unconditionally, by design. This is safe specifically because that Secret always lives in the controller's own release namespace — anyone able to create a Secret there could already replace the controller's Deployment, so an ownership check on this one Secret would not defend anything the namespace boundary doesn't already defend. The per-Gateway check above exists because that Secret lives in an arbitrary tenant namespace instead, where no such trust is implied. A per-Gateway config API certificate slot holding a Secret the Gateway does not own is skipped for the next slot rather than adopted. See [Config API Authentication](#config-api-authentication) and [Config API TLS](#config-api-tls).

    With config API TLS on (the default), the controller also holds `update` on exactly one Secret, the shared proxy's serving certificate, through a namespaced Role in the release namespace limited by `resourceNames`. The ClusterRole does not change.

!!! warning "Cluster-wide Secret access is an accepted risk"
    The controller holds `get`, `list`, `watch` and `create` on Secrets in **every namespace**. The three read verbs cannot be narrowed without giving up features the API itself defines, because three separate mechanisms put the Secrets in arbitrary namespaces: `GatewayClassConfig` is cluster-scoped and its `cloudflareCredentialsSecretRef.namespace` names whichever namespace it likes; Gateway and ListenerSet TLS `certificateRefs` resolve in the Gateway's namespace — wherever the tenant created it — and reach further still through a `ReferenceGrant`; and a per-Gateway data plane's connector-token Secret has to sit in the tenant namespace beside the proxy Deployment that mounts it, because a Pod cannot mount a Secret from another namespace. A label selector would only move the problem, since the controller cannot rely on a given label being present on a TLS Secret it did not create — cert-manager can set one through `Certificate.spec.secretTemplate`, but nothing makes an operator do so, and a Secret that misses the label is simply invisible to the controller.

    `create` is a weaker case and is granted unconditionally anyway. It exists so a data plane with no `authTokenSecretRef` gets a generated config-API bearer token instead of an unauthenticated config API, and so that with config API TLS on the controller can create its CA and each data plane's serving certificate. A deployment that supplies its own auth Secret everywhere and turns config API TLS off never exercises it. The grant is not conditioned on either today: the shared plane's need is known when the chart renders, but a per-Gateway plane's is a property of a `GatewayConfig` that may not exist yet, so there is no single template-time answer. `resourceNames` would not help either — RBAC does not apply it to `create`.

    Note that `list` and `watch` are not a milder grant than `get`: a List response carries the Secret contents, and the controller's informer cache holds every Secret in the cluster in memory. `create` is not read-only either — a Secret of type `kubernetes.io/service-account-token` carrying the `kubernetes.io/service-account.name` annotation is still populated with a working token by the control plane, so `create` together with the `get` above mints credentials for any ServiceAccount in any namespace.

    Concretely: anyone who can run code in the controller pod can read every Secret in the cluster and can escalate to any ServiceAccount in it. **The controller is a cluster-admin-equivalent workload** — restrict who can write to its release namespace, keep its ServiceAccount unshared, and verify the image signature before deploying (see [Container Image Verification](#container-image-verification)).

### Multi-Tenancy

Tenant isolation is layered: admission-level scoping (per-tenant listeners, `allowedListeners`/`allowedRoutes`, the opt-in hostname-ownership `ValidatingAdmissionPolicy`), an independent controller-side enforcement of the same hostname-ownership rule (a route that bypasses admission is still never programmed), and optional hard data-plane isolation with a dedicated proxy and tunnel per Gateway. A dedicated Gateway is accepted only once Cloudflare confirms that its connector token holds the tunnel it names, checked with the API credential that will write that tunnel's configuration, so naming a tunnel UUID is not enough to have the controller write to it. An operator can cap how many dedicated data planes one namespace may hold with `maxDataPlanesPerNamespace` on the GatewayClassConfig; past the cap the newest Gateways are refused and no plane is rendered for them. The boundaries and trade-offs are documented in the [Multi-Tenancy guide](../guides/multi-tenancy.md) and the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

!!! warning "GatewayConfig is workload-creation-equivalent"
    Because the controller renders Deployments for opted-in Gateways and `GatewayConfig.spec.image` selects the container image, **granting a user any write verb on `GatewayConfig` (plus a Gateway with `infrastructure.parametersRef`) is privilege-equivalent to granting `create` on Deployments in that namespace**: the controller becomes the deputy that runs the chosen image under the namespace's default ServiceAccount (neither proxy mounts an SA token, since neither calls the Kubernetes API). `update` and `patch` count as much as `create` here — `spec.image` on an existing object selects the image just as well, and the controller's `GatewayConfig` watch carries no generation predicate, so the edit re-renders at once. Treat RBAC on `gatewayconfigs` accordingly. A rendered data plane's config API is authenticated by default — the controller generates a per-Gateway bearer-token Secret when `authTokenSecretRef` is unset — and network-restricted by default — the controller renders a NetworkPolicy per data plane admitting the config API port only from the controller pod, not from the tenant's namespace and not from every pod sharing the controller's (set `proxy.networkPolicy.monitoringNamespaceSelector` to also admit your monitoring namespace for scraping). See the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

!!! warning "Writing `gateways/status` grants tunnel ownership"
    A tunnel belongs to whichever Gateway already advertises it in `Gateway.status.addresses`, so **`update` on `gateways/status` lets its holder claim a tunnel another namespace is serving** and evict the real owner, provided they also hold that tunnel's connector token. While the Cloudflare API cannot be reached, the address alone can hold a tunnel unverified, against any claim the controller has not confirmed since it last started. The Gateway API CRDs carry no RBAC aggregation labels, so the built-in `edit` and `admin` roles do not grant this; if you grant status write to tenants, tunnel ownership no longer holds for them. See [Per-Gateway Isolation](../guides/per-gateway-isolation.md).

### Container Security

The controller container follows security best practices:

| Setting | Value | Rationale |
|---------|-------|-----------|
| `runAsNonRoot` | `true` | Never run as root |
| `runAsUser` | `65534` | nobody user |
| `readOnlyRootFilesystem` | `true` | Prevent filesystem modifications |
| `allowPrivilegeEscalation` | `false` | Prevent privilege escalation |
| `capabilities.drop` | `ALL` | Drop all Linux capabilities |
| `seccompProfile.type` | `RuntimeDefault` | Use default seccomp profile |

### Network Security

#### Config API Authentication

The shared proxy's config API (where the controller pushes the routing table) is authenticated and network-restricted by default, matching the per-Gateway data planes described above. When `proxy.authTokenSecretRef.name` is left empty, the controller itself generates a random bearer token into a Secret (`<fullname>-proxy-auth-token`, where `<fullname>` is the Helm release fullname, typically `<release>-cloudflare-tunnel-gateway-controller`) on startup and uses it directly for its own push auth, and the proxy reads the same Secret via a pod-level `secretKeyRef`; the token is created once and reused on every restart, never rotated. Generating it via a live API call rather than at Helm template time means this is correct under GitOps controllers that render client-side with no cluster access (e.g. ArgoCD's default `helm template`), where a template-time `lookup` would silently mint a fresh value on every sync. `proxy.networkPolicy.enabled` (default `true`) additionally locks the config-API port to the controller pod: the ingress peer names it with a podSelector AND'd with the controller namespace, so a pod that merely shares that namespace is not admitted. Use `proxy.networkPolicy.ingress.from` to admit anything else, a monitoring stack scraping `/metrics` on the same port being the usual case. Set `proxy.authTokenSecretRef.name` to bring your own Secret instead — the controller resolves it through the same direct-API mechanism, never a `secretKeyRef` on its own pod, and never creates or modifies it: a missing bring-your-own Secret fails the controller closed rather than silently generating one at the operator's chosen name. Set `proxy.networkPolicy.enabled: false` to drop the NetworkPolicy on a cluster where it would be inert or unwanted — see the [Helm values reference](../configuration/helm-values.md).

The binary enforces this on its own side too, which matters for a hand-written proxy Deployment where no chart is wiring anything. In tunnel mode it refuses to start unless `PROXY_AUTH_TOKEN` is set, because its config API listens on every interface and one successful push replaces the entire routing table. A present-but-empty value is refused in either mode, since that is what a broken Secret produces rather than a decision to run open. `PROXY_ALLOW_UNAUTHENTICATED_CONFIG_API=1` opts out deliberately; it is a separate variable so a misconfiguration cannot spell the same thing as consent. Standalone mode, selected by omitting `TUNNEL_TOKEN`, keeps the unauthenticated default and binds loopback unless `PROXY_CONFIG_ADDR` or `PROXY_ADDR` asks for a wider address; once it does, the network boundary there is the operator's to provide.

The token and the NetworkPolicy are both reach controls, and neither encrypts anything. The push carries the bearer token in an `Authorization` header, plus the private key of the backend client certificate for any route whose parent Gateway sets `spec.tls.backend.clientCertificateRef` and whose backend is covered by a `BackendTLSPolicy`. By default both travel inside TLS, described in [Config API TLS](#config-api-tls) below. With `proxy.configAPITLS.enabled: false` the push is plain HTTP, and on a CNI without pod-to-pod encryption an on-path party inside the cluster reads the token and that key; wire confidentiality is then the cluster's to provide, through an encrypting CNI mode (WireGuard or IPsec) or a mesh that wraps pod-to-pod traffic in mTLS.

#### Config API TLS

The config API of the shared plane and of every per-Gateway plane is served over TLS 1.3 by default (`proxy.configAPITLS.enabled`). Nothing is generated at Helm template time: the controller creates everything through the API at runtime, so the chart renders the same under GitOps tools that render client-side.

- **The CA.** On startup the controller creates an ECDSA P-256 CA, valid for ten years, in the Secret `<fullname>-config-ca` in its release namespace, and reuses it on every later start. The CA key never leaves that namespace. A Secret at that name that does not hold a usable CA stops the controller at startup instead of being replaced, since a new CA would invalidate every certificate already issued.
- **One certificate per plane.** Each plane gets a serving certificate for its own config Service DNS name, valid for 365 days or until the CA expires, whichever comes first, and reissued once fewer than 120 days remain or once it no longer verifies. The shared certificate is checked once a replica wins leader election and hourly after that; a per-Gateway plane is checked on every reconcile, which runs at startup, hourly, and on every Secret change in its namespace. The shared plane's is `<fullname>-proxy-config-tls` in the release namespace; the controller updates it in place and the proxy re-reads the files on every handshake, so renewal needs no restart. A per-Gateway plane's is `cf-proxy-<gateway>-config-tls-<n>` in the Gateway's namespace, owned by the Gateway. For some Gateway names, such as one too long to fit the 63-character limit together with the suffix or one containing a dot, the part before `-<n>` is shortened and ends in a hash of the full name instead. The `-<n>` slot suffix is always kept. Outside its own namespace the controller only creates Secrets, so a renewal or replacement writes the next slot and rolls the plane onto it; earlier slots stay until the Gateway is deleted.
- **What the controller trusts.** The push trusts that CA alone, never the system roots, and requires the certificate to name the plane's config Service, the host of the configured endpoint, not the pod IP the push connects to. Each plane's connections are pooled apart from every other plane's.
- **What a tenant can do.** A tenant who can write Secrets in its own namespace can read and replace its own plane's certificate, and nothing more. A certificate it minted itself does not chain to the CA: the push to that plane is refused, the controller issues a fresh slot and reports `ConfigTLSCertificateReplaced` on the Gateway. The genuine certificate of its own plane names only that plane, so it cannot answer for anyone else's. GatewayConfig has no field that supplies a certificate or turns TLS off; only the chart value does.
- **Failures are loud.** The controller refuses to start when the CA Secret is unusable or the CA has expired, when only one of `--proxy-config-ca-secret` and `--proxy-config-tls-secret` is set, or when TLS is on and a `--proxy-endpoints` entry is `http://`. The proxy refuses to start when only one of `PROXY_CONFIG_TLS_CERT_FILE` and `PROXY_CONFIG_TLS_KEY_FILE` is set or the pair does not load, and the shared proxy waits in `ContainerCreating` until the controller has created its certificate. A sustained push failure caused by the handshake names the reason in the route's `ProxyConfigPushed=False` condition and its `ProxyConfigPushFailed` Event.
- **Probes and metrics.** They share the config API port and so move to HTTPS with it. kubelet does not verify a probe's certificate, and the chart's proxy ServiceMonitor scrapes with `insecureSkipVerify`, because the only copy of the CA certificate sits in the Secret that also holds the CA key.
- **Rotating the CA.** The CA is not rotated automatically. Once less than a year of its validity remains, the controller logs a warning at startup and the leader records a `ConfigTLSCAExpiring` Warning Event on the CA Secret on every issuance pass, which runs hourly, or every 30 seconds while issuance keeps failing. Past its expiry the controller refuses to start, and a running controller stops issuing per-Gateway certificates and reports the error on the Gateway. Delete `<fullname>-config-ca` and restart every controller replica, for example with `kubectl rollout restart`, since each replica loads the CA once at startup and one left running would keep issuing and trusting the old CA after a failover: every certificate then fails verification and is reissued, the shared one in place and each per-Gateway plane by rolling onto a new slot. Pushes to a plane fail until it serves its new certificate.

Outside the chart, the controller turns TLS on with `--proxy-config-ca-secret` and `--proxy-config-tls-secret` (both `<namespace>/<name>`), and the proxy serves TLS when `PROXY_CONFIG_TLS_CERT_FILE` and `PROXY_CONFIG_TLS_KEY_FILE` point at a mounted `kubernetes.io/tls` Secret. With none of them set, the controller creates no CA and both sides speak plain HTTP, which is also what standalone development runs use.

#### Forwarded Headers Reaching Backends

The proxy passes the forwarding headers it receives on to the backend instead of generating its own. A route's `RequestHeaderModifier` filter runs first and can set or remove any of them; a request mirror's copy sees only the filters listed before the mirror. What the filters leave is then handled as follows:

- `Forwarded`, `X-Forwarded-Host` and `X-Forwarded-Proto` reach the backend as the proxy received them.
- `X-Forwarded-For` reaches the backend with the address of the connection the request arrived on appended, when that address is known. That happens after the filter, so a removed `X-Forwarded-For` comes back holding only that address. A WebSocket upgrade and a request mirror's copy get the same treatment.
- The client's hop-by-hop headers, the RFC 7230 set (`Proxy-Authorization` among them) plus any header `Connection` names, are removed, apart from the forwarding headers above. `TE: trailers` is kept, and a WebSocket upgrade to a WebSocket-enabled backend carries `Connection: Upgrade` and `Upgrade` for the handshake.
- `X-Original-Host` and the proxy's internal `X-Proxy-Host-Rewritten` marker are removed before any request reaches a backend.
- Other end-to-end request headers pass through.

The Cloudflare edge forwards arbitrary `X-*` headers from any client, and apart from the removals above the proxy does not rewrite them, so a backend should not trust them as a statement from the edge:

- For the client IP, read `CF-Connecting-IP`, which the edge sets to the address of the client that connected to it. The leftmost `X-Forwarded-For` entry is whatever the client chose to send.
- Do not build absolute URLs, such as password-reset links, redirects or canonical tags, from `X-Forwarded-Host`. A client can set it to any host. Use a canonical host from the backend's own configuration.
- `X-Forwarded-Proto` is set by the edge to the protocol the client used to reach it, which is usually what a backend wants to know. See Cloudflare's [HTTP request headers](https://developers.cloudflare.com/fundamentals/reference/http-headers/) reference for what the edge sets.

#### Egress Requirements

The controller only needs egress to:

| Destination | Port | Purpose |
|-------------|------|---------|
| `api.cloudflare.com` | 443 | Cloudflare API |
| Kubernetes API | 443/6443 | Watch resources |
| Cluster DNS | 53 | Resolve the proxies' headless Service |
| Proxy config API | `proxy.configAPIPort` (8081) | Push the routing table to the data planes |
| OTLP collector | collector's port (4317 for OTLP/gRPC) | Export traces, only when `controller.tracing.enabled` |

Writing that as a policy runs into one thing worth knowing before you narrow anything. A rule with ports and no `to` permits those ports to every destination, and rules are OR'd, so one unrestricted rule makes every narrower rule beside it inert. The obvious fix — replacing it with a catch-all `ipBlock` — is not equivalent: Cilium does not match in-cluster identities through CIDR peers unless the agent runs with `--policy-cidr-match-mode`, which Cilium ships disabled and still marks beta, so a `0.0.0.0/0` peer denies a host-network API server on the self-managed clusters where that is exactly how the API server is reached. Narrow with a destination you have checked against your own CNI, and remember that most of them evaluate egress after DNAT, so a Service ClusterIP never matches.

The chart's `networkPolicy.kubernetesApiIpBlocks` is where that narrowing goes; it ships empty, meaning unrestricted.

The Cloudflare rule beside it is narrowed by `networkPolicy.cloudflareIpRanges`, which ships populated. Emptying both its address families is refused rather than rendered: the rule would lose its `to:` and become the unrestricted shape above, which is the opposite of what emptying an allowlist looks like it does. The refusal is armed by `networkPolicy.enabled` for the controller policy and by `proxy.networkPolicy.egressRestricted` for the proxy one, neither of which is on by default; turn off whichever of those two you have enabled to drop the restriction.

With `controller.tracing.enabled` on and a `controller.tracing.endpoint` that names an in-cluster Service with an explicit port (`<service>`, `<service>.<namespace>`, or `<service>.<namespace>.svc` with or without the cluster domain), the chart's policy admits that Service's namespace on the endpoint's port. A NetworkPolicy matches the port the collector pod listens on, so a collector Service whose `port` and `targetPort` differ stays blocked. Any other endpoint gets no rule. A loopback endpoint needs none, and neither does an empty one: the chart sets no OTLP environment variable, so the exporter falls back to `localhost:4317`. A collector outside the cluster, an IP address, or an endpoint without a port is reached through the Kubernetes API rule while it listens on 443 and that rule is not narrowed; otherwise add a NetworkPolicy of your own that admits it, since policies selecting the same pod are additive.

#### NetworkPolicy Example

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cloudflare-tunnel-gateway-controller
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: cloudflare-tunnel-gateway-controller
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Prometheus scraping
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
      ports:
        - port: 8080
  egress:
    # Kubernetes API. No `to` at all, which permits these ports everywhere —
    # the same thing the chart renders by default, for the reasons above. Until
    # you narrow it the Cloudflare rule below has no effect.
    - ports:
        - port: 443
        - port: 6443
    # Cloudflare API. Abbreviated — the full list is at
    # https://www.cloudflare.com/ips/ and both families belong here. Narrowing
    # the rule above without completing this one breaks Cloudflare API calls.
    - to:
        - ipBlock:
            cidr: 173.245.48.0/20
      ports:
        - port: 443
    # The proxies' config API. Without this no data plane receives a routing
    # table, and Gateway status stays clean while that is true. The first peer
    # is the shared plane in this namespace; the second is per-Gateway planes,
    # which live in their Gateway's namespace.
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: cloudflare-tunnel-gateway-controller-proxy
              app.kubernetes.io/component: proxy
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              app.kubernetes.io/name: cloudflare-tunnel-gateway-proxy
      ports:
        - port: 8081
    # DNS
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```

This is a starting point for operators applying manifests by hand, not a transcript of what the chart renders: the chart's version is generated from values and carries knobs this example flattens. Read `templates/networkpolicy.yaml` if you need the exact shape.

## Supply Chain Security

### Container Image Verification

Container images are signed with cosign (keyless). A tag is a mutable pointer, so verifying a tag and later deploying that same tag are two separate resolutions and can land on different artifacts. Ask cosign which digest it actually verified, and deploy that one.

The chart deploys **two** images from two repositories, and both are signed by the same release job, so verify both and keep the two digests apart:

```bash
for repo in cloudflare-tunnel-gateway-controller cloudflare-tunnel-gateway-controller-proxy; do
  digest=$(cosign verify "ghcr.io/lexfrei/${repo}:<version>" \
    --certificate-identity-regexp="https://github.com/lexfrei/cloudflare-tunnel-gateway-controller" \
    --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
    --output=json | jq -r '.[0].critical.image."docker-manifest-digest"')
  printf '%s: %s\n' "$repo" "${digest:-VERIFICATION FAILED}"
done
```

Note that `<version>` carries no `v` prefix: the release tag `v1.2.3` publishes image tag `1.2.3`, and `cosign` reports `MANIFEST_UNKNOWN` if you paste the prefixed form.

Feed each digest to its own slot. `image.digest` and `proxy.image.digest` take precedence over the tag, and the proxy digest is carried into the controller's `--proxy-image` flag, so it reaches the data planes the controller renders for per-Gateway isolation as well as the ones Helm renders:

```yaml
image:
  digest: "sha256:..."     # cloudflare-tunnel-gateway-controller
proxy:
  image:
    digest: "sha256:..."   # cloudflare-tunnel-gateway-controller-proxy
```

!!! warning "A per-Gateway data plane can override the pin"
    `proxy.image.digest` reaches a rendered per-Gateway data plane as its **default**, not as a constraint. If that Gateway's `GatewayConfig` sets `spec.image`, the controller uses it and the pin does not apply to that plane. This follows from `GatewayConfig` being workload-creation-equivalent (see [Multi-Tenancy](#multi-tenancy)) — whoever may write one already chooses what runs there.

    Making the pin hold cluster-wide means restricting **every write verb** on `gatewayconfigs`, not just `create`: `update` and `patch` set `spec.image` on an existing object just as well, and the controller's `GatewayConfig` watch carries no generation predicate, so the edit re-renders the Deployment immediately. Setting `spec.image` to the pinned digest in each `GatewayConfig` has the same dependency — it holds only for as long as nobody can update them afterwards.

Pinning by digest also makes `pullPolicy` irrelevant to correctness: a digest reference always resolves to the same bytes, so a node cache can never serve a different image than the one you verified. The trade-off is that upgrades stop being automatic — you resolve and verify a new digest for each release.

### Helm Chart Verification

The chart is published as an OCI artifact, and the release job signs that artifact with cosign (keyless) as it does the images. No `.prov` provenance file is published, so `helm verify` has nothing to check. Verify the OCI artifact instead, and for the same reason as with the images, install the digest cosign verified rather than the version tag, which would be resolved a second time:

```bash
digest=$(cosign verify "ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller:<version>" \
  --certificate-identity-regexp="https://github.com/lexfrei/cloudflare-tunnel-gateway-controller" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  --output=json | jq -r '.[0].critical.image."docker-manifest-digest"')
helm install <release> "oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller@${digest:?cosign verification failed}" \
  --namespace <namespace> --values <values-file>
```

The chart version, like the image tag, is the release version without its leading `v`. If your Helm does not accept a chart reference by digest, `helm pull oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller --version <version>` prints the `Digest:` it pulled; compare it with the one cosign verified, then install the pulled archive.

## Secrets in Logs

The controller is designed to never log sensitive information:

- API tokens are not logged
- Tunnel tokens are not logged
- Secret contents are not logged

!!! warning "Report Log Leaks"

    If you find sensitive data in logs, please report it as a security issue.

## Security Scanning

The project uses automated security scanning:

| Tool | Purpose |
|------|---------|
| Trivy | Vulnerability scanning in CI |
| gosec | Go security linter |
| Dependabot/Renovate | Dependency updates |

## Incident Response

If you believe the controller has been compromised:

1. **Revoke Cloudflare API token** immediately
2. **Delete the controller deployment**
3. **Review Cloudflare audit logs** for unauthorized changes
4. **Rotate tunnel credentials** if needed
5. **Report the incident** via security email

## Secure Deployment Checklist

- [ ] API token stored in Kubernetes Secret (not in values.yaml)
- [ ] API token has minimal required permissions
- [ ] Controller running as non-root
- [ ] Read-only root filesystem enabled
- [ ] NetworkPolicy restricting egress
- [ ] ServiceAccount with minimal RBAC
- [ ] `proxy.allowXOriginalHost` left off (the default) — it exists for the conformance suite, and trusting that header lets a client be served by a different hostname's backend
- [ ] Container image verified with cosign
- [ ] Prometheus monitoring enabled
- [ ] Alerts configured for anomalous behavior
