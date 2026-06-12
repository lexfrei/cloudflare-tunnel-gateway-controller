# Security

This document covers the security policy and best practices for the Cloudflare Tunnel Gateway Controller.

## Supported Versions

| Version | Supported |
|---------|-----------|
| 3.x.x | Yes |
| 2.x.x | No |

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
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets", "configmaps"]
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
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]

  # GatewayConfig CRD - per-Gateway data-plane parameters referenced from
  # Gateway.spec.infrastructure.parametersRef
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["gatewayconfigs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["cf.k8s.lex.la"]
    resources: ["gatewayconfigs/status"]
    verbs: ["get", "update", "patch"]

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

### Multi-Tenancy

Tenant isolation is layered: admission-level scoping (per-tenant listeners, `allowedListeners`/`allowedRoutes`, the opt-in hostname-ownership `ValidatingAdmissionPolicy`), an independent controller-side enforcement of the same hostname-ownership rule (a route that bypasses admission is still never programmed), and optional hard data-plane isolation with a dedicated proxy and tunnel per Gateway. The boundaries and trade-offs are documented in the [Multi-Tenancy guide](../guides/multi-tenancy.md) and the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

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

#### Egress Requirements

The controller only needs egress to:

| Destination | Port | Purpose |
|-------------|------|---------|
| `api.cloudflare.com` | 443 | Cloudflare API |
| Kubernetes API | 443/6443 | Watch resources |

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
    # Kubernetes API and Cloudflare API
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - port: 443
        - port: 6443
    # DNS
    - to: []
      ports:
        - port: 53
          protocol: UDP
```

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
helm verify cloudflare-tunnel-gateway-controller-1.0.0.tgz
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
- [ ] Container image verified with cosign
- [ ] Prometheus monitoring enabled
- [ ] Alerts configured for anomalous behavior
