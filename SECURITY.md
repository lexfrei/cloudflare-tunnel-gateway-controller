# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 0.x.x   | :white_check_mark: |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report vulnerabilities via email:

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

- **Initial Response**: Within 48 hours
- **Status Update**: Within 7 days
- **Fix Timeline**: Depends on severity

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
   - Account: Account Settings (Read) - only if auto-detecting account ID

3. **Rotated regularly**
   - Create new token in Cloudflare dashboard
   - Update Kubernetes secret
   - Controller picks up new token on restart

4. **Never committed to git**
   - Use external secret management (Vault, AWS Secrets Manager)
   - See `charts/.../examples/external-secrets-values.yaml`

### RBAC Configuration

The controller requires specific Kubernetes permissions:

```yaml
# Minimum required permissions
rules:
  # Gateway API - read specs, write status
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways", "httproutes", "gatewayclasses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways/status", "httproutes/status"]
    verbs: ["get", "update", "patch"]

  # Services - read only for backend resolution
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "watch"]

  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

Additional permissions are needed only if `--manage-cloudflared` is enabled.

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
    # Kubernetes API
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

### Supply Chain Security

#### Container Image Verification

Container images are signed with cosign (keyless):

```bash
# Verify image signature
cosign verify ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller:latest \
  --certificate-identity-regexp="https://github.com/lexfrei/cloudflare-tunnel-gateway-controller" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"
```

#### Helm Chart Verification

```bash
# Verify chart provenance
helm verify cloudflare-tunnel-gateway-controller-0.1.0.tgz
```

### Secrets in Logs

The controller is designed to never log sensitive information:

- API tokens are not logged
- Tunnel tokens are not logged
- Secret contents are not logged

If you find sensitive data in logs, please report it as a security issue.

## Security Scanning

The project uses automated security scanning:

- **Trivy**: Vulnerability scanning in CI
- **gosec**: Go security linter
- **Dependabot/Renovate**: Dependency updates

## Incident Response

If you believe the controller has been compromised:

1. **Revoke Cloudflare API token** immediately
2. **Delete the controller deployment**
3. **Review Cloudflare audit logs** for unauthorized changes
4. **Rotate tunnel credentials** if needed
5. **Report the incident** via security email
