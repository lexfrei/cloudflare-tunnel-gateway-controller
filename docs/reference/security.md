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
# Minimum required permissions (v3) -- matches charts/.../templates/clusterrole.yaml
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
  # CustomResourceDefinitions - single Get of the gatewayclasses CRD to read
  # the bundle-version annotation for the SupportedVersion condition
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
  # Secrets - read for credentials; create for the generated config-API auth Secret, both the per-Gateway one and the shared plane's (no update/delete: token never rotated).
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

  # Deployments - the proxy Secret reconciler patches the proxy Deployment's
  # pod-template annotation to roll pods when the tunnel-token Secret rotates,
  # and the per-Gateway data planes render a dedicated proxy Deployment per
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
    The controller reads Secrets and ConfigMaps and writes status subresources. Its workload writes are scoped to the data planes it owns: patching the shared proxy Deployment's pod-template annotation when the tunnel-token Secret rotates (a native rolling restart), and rendering a dedicated proxy Deployment, headless config Service, and optional HorizontalPodAutoscaler for each Gateway opted into a per-Gateway data plane via `infrastructure.parametersRef`. Those rendered objects are controller-owned via ownerReferences, kept in sync against drift, and deleted only when actually owned — a name collision with a user resource can never turn into a deletion. Workload write access is cluster-wide because Gateways live in arbitrary namespaces.

    Because the RBAC grant for these resources is broad (`delete` on Deployments, Services, and HorizontalPodAutoscalers cluster-wide), the **in-code ownership check is the security boundary, not the RBAC scope**. Every apply and create path for a *per-Gateway* rendered object — including that plane's generated config-API auth Secret — refuses to adopt, update, or GC an object at a rendered name unless it already carries this Gateway's controller ownerReference. A pre-existing object with a foreign owner (or none) is left untouched and the reconcile surfaces a `RenderFailed` event instead of overwriting it. The *shared* plane's generated auth Secret (below) has no Gateway to check ownership against, so it has no equivalent adoption check: it reuses whatever Secret already exists at its deterministic name unconditionally, by design. This is safe specifically because that Secret always lives in the controller's own release namespace — anyone able to create a Secret there could already replace the controller's Deployment, so an ownership check on this one Secret would not defend anything the namespace boundary doesn't already defend. The per-Gateway check above exists because that Secret lives in an arbitrary tenant namespace instead, where no such trust is implied. See [Config API Authentication](#config-api-authentication).

### Multi-Tenancy

Tenant isolation is layered: admission-level scoping (per-tenant listeners, `allowedListeners`/`allowedRoutes`, the opt-in hostname-ownership `ValidatingAdmissionPolicy`), an independent controller-side enforcement of the same hostname-ownership rule (a route that bypasses admission is still never programmed), and optional hard data-plane isolation with a dedicated proxy and tunnel per Gateway. An operator can cap how many dedicated data planes one namespace may hold with `maxDataPlanesPerNamespace` on the GatewayClassConfig; past the cap the newest Gateways are refused and no plane is rendered for them. The boundaries and trade-offs are documented in the [Multi-Tenancy guide](../guides/multi-tenancy.md) and the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

!!! warning "GatewayConfig is workload-creation-equivalent"
    Because the controller renders Deployments for opted-in Gateways and `GatewayConfig.spec.image` selects the container image, **granting a user `create` on `GatewayConfig` (plus a Gateway with `infrastructure.parametersRef`) is privilege-equivalent to granting `create` on Deployments in that namespace**: the controller becomes the deputy that runs the chosen image under the namespace's default ServiceAccount (neither proxy mounts an SA token, since neither calls the Kubernetes API). Treat RBAC on `gatewayconfigs` accordingly. A rendered data plane's config API is authenticated by default — the controller generates a per-Gateway bearer-token Secret when `authTokenSecretRef` is unset — and network-restricted by default — the controller renders a NetworkPolicy per data plane admitting the config API port only from the controller pod, not from the tenant's namespace and not from every pod sharing the controller's (set `proxy.networkPolicy.monitoringNamespaceSelector` to also admit your monitoring namespace for scraping). See the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

!!! warning "Writing `gateways/status` grants tunnel ownership"
    A tunnel belongs to whichever Gateway already advertises it in `Gateway.status.addresses`, so **`update` on `gateways/status` lets its holder claim a tunnel another namespace is serving** and evict the real owner. The Gateway API CRDs carry no RBAC aggregation labels, so the built-in `edit` and `admin` roles do not grant this; if you grant status write to tenants, tunnel ownership no longer holds for them. See [Per-Gateway Isolation](../guides/per-gateway-isolation.md).

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

The binary enforces this on its own side too, which matters for a hand-written proxy Deployment where no chart is wiring anything. In tunnel mode it refuses to start unless `PROXY_AUTH_TOKEN` is set, because its config API listens on every interface and one successful push replaces the entire routing table. A present-but-empty value is refused in either mode, since that is what a broken Secret produces rather than a decision to run open. `PROXY_ALLOW_UNAUTHENTICATED_CONFIG_API=1` opts out deliberately; it is a separate variable so a misconfiguration cannot spell the same thing as consent. Standalone mode, selected by omitting `TUNNEL_TOKEN`, keeps the unauthenticated default and binds the same interfaces, so the network boundary there is the operator's to provide.

The token and the NetworkPolicy are both reach controls, and neither encrypts anything. The push is plain HTTP — the endpoint the chart wires for the shared plane and the ones the controller renders for per-Gateway planes are both `http://` URLs — and it carries the bearer token in an `Authorization` header, plus the private key of the backend client certificate for any route whose parent Gateway sets `spec.tls.backend.clientCertificateRef` and whose backend is covered by a `BackendTLSPolicy`. So the token answers who may replace the routing table and the NetworkPolicy answers who may reach the port at all; neither says anything about who can read the bytes in flight, and on a CNI without pod-to-pod encryption an on-path party inside the cluster reads the token and that key. Wire confidentiality is the cluster's to provide: an encrypting CNI mode (WireGuard or IPsec), or a mesh that wraps pod-to-pod traffic in mTLS. TLS on the config API itself is tracked in [#719](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/719).

#### Egress Requirements

The controller only needs egress to:

| Destination | Port | Purpose |
|-------------|------|---------|
| `api.cloudflare.com` | 443 | Cloudflare API |
| Kubernetes API | 443/6443 | Watch resources |
| Cluster DNS | 53 | Resolve the proxies' headless Service |
| Proxy config API | `proxy.configAPIPort` (8081) for the shared plane, always 8081 for per-Gateway planes | Push the routing table to the data planes |
| OTLP collector | collector's port (4317 for OTLP/gRPC) | Export traces, only when `tracing.enabled` |

Writing that as a policy runs into one thing worth knowing before you narrow anything. A rule with ports and no `to` permits those ports to every destination, and rules are OR'd, so one unrestricted rule makes every narrower rule beside it inert. The obvious fix — replacing it with a catch-all `ipBlock` — is not equivalent: Cilium does not match in-cluster identities through CIDR peers unless the agent runs with `--policy-cidr-match-mode`, which Cilium ships disabled and still marks beta, so a `0.0.0.0/0` peer denies a host-network API server on the self-managed clusters where that is exactly how the API server is reached. Narrow with a destination you have checked against your own CNI, and remember that most of them evaluate egress after DNAT, so a Service ClusterIP never matches.

The chart's `networkPolicy.kubernetesApiIpBlocks` is where that narrowing goes; it ships empty, meaning unrestricted.

The chart's policy carries no rule for the collector, so turning tracing on under it drops the exporter's traffic. Add the rule yourself.

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

Container images are signed with cosign (keyless):

```bash
cosign verify ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller:latest \
  --certificate-identity-regexp="https://github.com/lexfrei/cloudflare-tunnel-gateway-controller" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"
```

### Helm Chart Verification

```bash
helm verify cloudflare-tunnel-gateway-controller-<version>.tgz
```

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
