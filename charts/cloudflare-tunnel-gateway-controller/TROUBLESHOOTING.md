# Troubleshooting Guide

This guide helps diagnose and resolve common issues with the Cloudflare Tunnel Gateway Controller Helm chart.

## Table of Contents

- [Installation Issues](#installation-issues)
- [Pod Startup Problems](#pod-startup-problems)
- [Authentication and API Issues](#authentication-and-api-issues)
- [Network Connectivity](#network-connectivity)
- [AmneziaWG Sidecar Issues](#amneziawg-sidecar-issues)
- [Gateway API Resources](#gateway-api-resources)
- [Monitoring and Health Checks](#monitoring-and-health-checks)
- [Performance Issues](#performance-issues)

## Installation Issues

### Schema Validation Errors

**Problem**: `values don't meet the specifications of the schema`

**Common causes**:

1. Empty or invalid `tunnelId`:

   ```text
   Error: at '/cloudflare/tunnelId': minLength: got 0, want 1
   ```

   **Solution**: Provide a valid Tunnel ID from Cloudflare Zero Trust Dashboard

2. Invalid characters in `tunnelId`:

   ```text
   Error: at '/cloudflare/tunnelId': pattern mismatch
   ```

   **Solution**: Use only alphanumeric characters and hyphens (a-zA-Z0-9-)

3. Missing API credentials:

   ```text
   Error: neither apiToken nor apiTokenSecretName provided
   ```

   **Solution**: Set either `cloudflare.apiToken` or `cloudflare.apiTokenSecretName`

**Verification**:

```bash
# Validate values before installation
helm lint charts/cloudflare-tunnel-gateway-controller -f my-values.yaml

# Dry-run installation to catch errors
helm install --dry-run --debug my-release \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  -f my-values.yaml
```

### Helm Repository Issues

**Problem**: Chart not found or authentication errors

**Solution**:

```bash
# Ensure you're using the correct OCI registry URL
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --version 0.1.0

# For private registries, authenticate first
echo $GITHUB_TOKEN | helm registry login ghcr.io --username USERNAME --password-stdin
```

## Pod Startup Problems

### CrashLoopBackOff

**Diagnosis**:

```bash
# Check pod status
kubectl get pods -n cloudflare-tunnel-system

# View pod logs
kubectl logs -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

# Check events
kubectl describe pod -n cloudflare-tunnel-system \
  $(kubectl get pod -n cloudflare-tunnel-system -l app.kubernetes.io/name=cloudflare-tunnel-gateway-controller -o name | head -1)
```

**Common causes**:

1. **Invalid API Token**:

   ```text
   Error: authentication failed: invalid API token
   ```

   **Solution**: Verify token has correct scopes (Account.Cloudflare Tunnel:Edit, Zone.DNS:Edit)

2. **Missing Secret**:

   ```text
   Error: secret "cloudflare-credentials" not found
   ```

   **Solution**: Create the secret or update `apiTokenSecretName` in values

3. **Read-only filesystem errors**:

   ```text
   Error: cannot create directory: read-only file system
   ```

   **Solution**: Ensure temporary directories are writable (check emptyDir volumes in deployment)

### ImagePullBackOff

**Diagnosis**:

```bash
kubectl describe pod -n cloudflare-tunnel-system POD_NAME
```

**Common causes**:

1. Invalid image tag or repository
2. Missing image pull secrets for private registries
3. Network issues preventing image download

**Solution**:

```yaml
# Use correct image settings
image:
  repository: ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller
  tag: ""  # Uses appVersion from Chart.yaml
  pullPolicy: IfNotPresent

# For private registries
imagePullSecrets:
  - name: ghcr-credentials
```

### StartupProbe Failures

**Problem**: Pod killed during startup by startupProbe

**Diagnosis**:

```bash
kubectl describe pod -n cloudflare-tunnel-system POD_NAME | grep -A 10 "Startup"
```

**Solution**: Increase startup time for slow environments

```yaml
# In values.yaml - NOT recommended to override defaults
# Startup probe allows 60 seconds (12 failures × 5 second period)
# Adjust only if genuinely needed
```

## Authentication and API Issues

### Invalid Cloudflare API Token

**Symptoms**:

- Pods crash with authentication errors
- Logs show `401 Unauthorized` or `403 Forbidden`

**Diagnosis**:

```bash
# Test API token manually
export CF_API_TOKEN="your-token"
curl -H "Authorization: Bearer $CF_API_TOKEN" \
  https://api.cloudflare.com/client/v4/user/tokens/verify
```

**Solution**:

1. Create new API token with required scopes:
   - Account.Cloudflare Tunnel:Edit
   - Zone.DNS:Edit

2. Update secret:

   ```bash
   kubectl create secret generic cloudflare-credentials \
     --from-literal=api-token="NEW_TOKEN" \
     --namespace cloudflare-tunnel-system \
     --dry-run=client -o yaml | kubectl apply -f -

   # Force pod restart to pick up new secret
   kubectl rollout restart deployment/cloudflare-tunnel-gateway-controller \
     -n cloudflare-tunnel-system
   ```

### External Secrets Operator Issues

**Problem**: ExternalSecret not syncing

**Diagnosis**:

```bash
# Check ExternalSecret status
kubectl describe externalsecret cloudflare-api-token -n cloudflare-tunnel-system

# Check SecretStore connectivity
kubectl describe secretstore aws-secretsmanager -n cloudflare-tunnel-system

# View ESO operator logs
kubectl logs -n external-secrets-system \
  deployment/external-secrets
```

**Common solutions**:

1. **IAM/RBAC permissions**: Ensure service account has access to secret backend
2. **Network policies**: Allow ESO to reach external API (AWS, GCP, etc.)
3. **Secret path**: Verify `remoteRef.key` points to correct secret
4. **Refresh interval**: Check if secret needs manual refresh

## Network Connectivity

### NetworkPolicy Blocking Traffic

**Symptoms**:

- Metrics not accessible from Prometheus
- Health checks failing
- Cannot communicate with Cloudflare API

**Diagnosis**:

```bash
# Check NetworkPolicy rules
kubectl get networkpolicy -n cloudflare-tunnel-system

# Test connectivity from a debug pod
kubectl run debug --rm -it --image=nicolaka/netshoot \
  --namespace cloudflare-tunnel-system -- bash

# Inside debug pod:
curl http://cloudflare-tunnel-gateway-controller:8080/metrics
curl http://cloudflare-tunnel-gateway-controller:8081/healthz
curl -I https://api.cloudflare.com
```

**Solution**: Adjust NetworkPolicy ingress selectors

```yaml
networkPolicy:
  enabled: true
  ingress:
    from:
      - namespaceSelector:
          matchLabels:
            name: monitoring
      - namespaceSelector: {}  # Allow all namespaces (less secure)
```

### DNS Resolution Issues

**Symptoms**:

- Cannot resolve Cloudflare API endpoints
- Errors: `no such host` or `dial tcp: lookup failed`

**Diagnosis**:

```bash
# Check pod DNS configuration
kubectl exec -n cloudflare-tunnel-system POD_NAME -- cat /etc/resolv.conf

# Test DNS resolution
kubectl exec -n cloudflare-tunnel-system POD_NAME -- \
  nslookup api.cloudflare.com
```

**Solution**: Configure custom DNS

```yaml
dnsPolicy: "None"
dnsConfig:
  nameservers:
    - 1.1.1.1
    - 8.8.8.8
  searches:
    - cloudflare-tunnel-system.svc.cluster.local
    - svc.cluster.local
    - cluster.local
  options:
    - name: ndots
      value: "2"
```

### Cloudflare API Unreachable

**Diagnosis**:

```bash
# From controller pod
kubectl exec -n cloudflare-tunnel-system POD_NAME -- \
  curl -v https://api.cloudflare.com

# Check egress rules
kubectl get networkpolicy -n cloudflare-tunnel-system -o yaml | \
  grep -A 20 "egress"
```

**Solution**: Ensure NetworkPolicy allows egress to Cloudflare IPs (see chart's networkpolicy.yaml for IP ranges)

## AmneziaWG Sidecar Issues

### AWG Interface Creation Failures

**Symptoms**:

- Container crash with `Operation not permitted`
- Interface conflicts: `device already exists`

**Diagnosis**:

```bash
# Check security context
kubectl get deployment -n cloudflare-tunnel-system \
  cloudflare-tunnel-gateway-controller -o yaml | \
  grep -A 10 "securityContext"

# View AWG sidecar logs
kubectl logs -n cloudflare-tunnel-system POD_NAME -c amneziawg
```

**Solutions**:

1. **Permission issues**: AWG requires `NET_ADMIN` capability (chart handles this automatically)

2. **Interface name conflicts**:

   Interface names are now auto-generated using kernel numbering.
   Use different prefixes for different GatewayClassConfigs:

   ```yaml
   awg:
     interfacePrefix: "awg-prod"  # Will create awg-prod0, awg-prod1, etc.
   ```

3. **Missing secret**:

   ```bash
   kubectl create secret generic awg-config \
     --from-file=wg0.conf=/path/to/config \
     --namespace cloudflare-tunnel-system
   ```

### Graceful Shutdown Issues

**Problem**: AWG interface not cleaned up on pod termination

**Diagnosis**:

```bash
# Check preStop hook
kubectl get deployment -n cloudflare-tunnel-system \
  cloudflare-tunnel-gateway-controller -o yaml | \
  grep -A 20 "preStop"
```

**Solution**: Chart includes preStop hook to delete interface. If issues persist:

```yaml
terminationGracePeriodSeconds: 60  # Increase if cleanup takes time
```

## Gateway API Resources

### Gateway Not Ready

**Diagnosis**:

```bash
# Check Gateway status
kubectl get gateway -A
kubectl describe gateway my-gateway -n my-namespace

# Check controller logs for Gateway events
kubectl logs -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller | \
  grep -i gateway
```

**Common causes**:

1. **GatewayClass not found**:

   ```yaml
   gatewayClass:
     create: true  # Ensure GatewayClass is created
   ```

2. **Wrong controller name in Gateway spec**:

   ```yaml
   # Gateway must reference correct GatewayClass
   spec:
     gatewayClassName: cloudflare-tunnel  # Must match chart's gatewayClassName
   ```

3. **Service backend not found**: Ensure referenced Services exist

### HTTPRoute Not Attached

**Diagnosis**:

```bash
kubectl describe httproute my-route -n my-namespace
```

**Common causes**:

1. **Namespace mismatch**: HTTPRoute must be in same namespace as referenced Gateway (or use ReferenceGrant)
2. **Invalid hostname**: Check hostname patterns match Gateway listeners
3. **Backend service not found**: Verify Service exists and has matching ports

### Status Not Updating

**Problem**: Gateway/HTTPRoute status conditions not updating

**Diagnosis**:

```bash
# Check RBAC permissions
kubectl auth can-i update gateways/status \
  --as=system:serviceaccount:cloudflare-tunnel-system:cloudflare-tunnel-gateway-controller

# View controller logs
kubectl logs -n cloudflare-tunnel-system deployment/cloudflare-tunnel-gateway-controller
```

**Solution**: Ensure ClusterRole has status subresource permissions (chart includes this by default)

## Monitoring and Health Checks

### Metrics Not Available

**Problem**: Prometheus cannot scrape metrics

**Diagnosis**:

```bash
# Test metrics endpoint
kubectl port-forward -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller 8080:8080

curl http://localhost:8080/metrics
```

**Solutions**:

1. **Service annotations** (for annotation-based discovery):

   ```yaml
   service:
     annotations:
       prometheus.io/scrape: "true"
       prometheus.io/port: "8080"
       prometheus.io/path: "/metrics"
   ```

2. **ServiceMonitor** (for Prometheus Operator):

   ```yaml
   serviceMonitor:
     enabled: true
     labels:
       prometheus: kube-prometheus  # Match Prometheus selector
   ```

3. **NetworkPolicy**: Ensure Prometheus can reach the service

### Health Checks Failing

**Diagnosis**:

```bash
# Test health endpoints
kubectl port-forward -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller 8081:8081

curl http://localhost:8081/healthz
curl http://localhost:8081/readyz
```

**Common causes**:

1. **Incorrect port configuration**
2. **Resource starvation** (CPU/memory throttling)
3. **Cloudflare API connectivity issues**

## Performance Issues

### High Memory Usage

**Symptoms**:

- Pods OOMKilled
- Memory usage growing over time

**Diagnosis**:

```bash
# Check resource usage
kubectl top pod -n cloudflare-tunnel-system

# View memory limits
kubectl get deployment -n cloudflare-tunnel-system \
  cloudflare-tunnel-gateway-controller -o yaml | \
  grep -A 5 "resources"
```

**Solutions**:

1. **Increase memory limits**:

   ```yaml
   resources:
     limits:
       memory: 512Mi  # Increase from default 256Mi
     requests:
       memory: 256Mi
   ```

2. **Check for memory leaks** in controller logs
3. **Reduce watched resource count** if managing many Gateways/HTTPRoutes

### High CPU Usage

**Diagnosis**:

```bash
kubectl top pod -n cloudflare-tunnel-system
```

**Solutions**:

1. **Increase CPU limits**:

   ```yaml
   resources:
     limits:
       cpu: 500m  # Increase from default 200m
   ```

2. **Increase replicas** for horizontal scaling:

   ```yaml
   replicaCount: 3
   podDisruptionBudget:
     enabled: true
     minAvailable: 2
   ```

### Slow Reconciliation

**Problem**: Changes to Gateway/HTTPRoute take long to apply

**Diagnosis**:

```bash
# Check controller logs for reconciliation times
kubectl logs -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller | \
  grep -i reconcil
```

**Solutions**:

1. **Check Cloudflare API rate limits** in logs
2. **Verify network latency** to Cloudflare API
3. **Ensure sufficient resources** (CPU not throttled)

## Getting More Help

### Enable Debug Logging

```yaml
controller:
  logLevel: "debug"  # Change from "info"
  logFormat: "json"
```

After updating:

```bash
helm upgrade cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  -f values.yaml \
  -n cloudflare-tunnel-system

# View debug logs
kubectl logs -f -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller
```

### Collect Diagnostic Information

```bash
# Pod status and events
kubectl get pods -n cloudflare-tunnel-system -o wide
kubectl describe pod -n cloudflare-tunnel-system POD_NAME

# Recent logs
kubectl logs --tail=100 -n cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

# Resource usage
kubectl top pod -n cloudflare-tunnel-system

# Network policies
kubectl get networkpolicy -n cloudflare-tunnel-system -o yaml

# Gateway API resources
kubectl get gatewayclasses,gateways,httproutes -A
```

### Reporting Issues

When reporting issues, include:

1. Helm chart version: `helm list -n cloudflare-tunnel-system`
2. Kubernetes version: `kubectl version`
3. Cloud provider and CNI plugin
4. Relevant pod logs (sanitize secrets!)
5. Gateway/HTTPRoute manifests (sanitize sensitive data)
6. Output of diagnostic commands above

Report issues at: <https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues>
