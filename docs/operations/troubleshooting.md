# Troubleshooting

This guide helps diagnose and resolve common issues with the Cloudflare Tunnel
Gateway Controller.

## Quick Diagnostics

```bash
# Check controller status
kubectl get pods --namespace cloudflare-tunnel-system

# View controller logs
kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

# Check Gateway status
kubectl get gateway --all-namespaces

# Check HTTPRoute status
kubectl get httproute --all-namespaces
```

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

    **Solution**: Use only alphanumeric characters and hyphens

3. Missing API credentials:

    ```text
    Error: neither apiToken nor apiTokenSecretName provided
    ```

    **Solution**: Set either `cloudflare.apiToken` or `cloudflare.apiTokenSecretName`

**Verification**:

```bash
# Validate values before installation
helm lint charts/cloudflare-tunnel-gateway-controller --values my-values.yaml

# Dry-run installation
helm install --dry-run --debug my-release \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --values my-values.yaml
```

## Pod Startup Problems

### CrashLoopBackOff

**Diagnosis**:

```bash
kubectl get pods --namespace cloudflare-tunnel-system

kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

kubectl describe pod --namespace cloudflare-tunnel-system \
  --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller
```

**Common causes**:

| Error | Cause | Solution |
|-------|-------|----------|
| `authentication failed` | Invalid API token | Verify token scopes |
| `secret not found` | Missing secret | Create required secret |
| `read-only file system` | Security context issue | Check emptyDir volumes |

### ImagePullBackOff

**Diagnosis**:

```bash
kubectl describe pod --namespace cloudflare-tunnel-system POD_NAME
```

**Solutions**:

```yaml
image:
  repository: ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller
  tag: ""  # Uses appVersion from Chart.yaml
  pullPolicy: IfNotPresent

# For private registries
imagePullSecrets:
  - name: ghcr-credentials
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
curl --header "Authorization: Bearer $CF_API_TOKEN" \
  https://api.cloudflare.com/client/v4/user/tokens/verify
```

**Solution**:

1. Create new API token with required scopes:
   - Account.Cloudflare Tunnel:Edit

2. Update secret:

    ```bash
    kubectl create secret generic cloudflare-credentials \
      --from-literal=api-token="NEW_TOKEN" \
      --namespace cloudflare-tunnel-system \
      --dry-run=client --output yaml | kubectl apply --filename -

    kubectl rollout restart deployment/cloudflare-tunnel-gateway-controller \
      --namespace cloudflare-tunnel-system
    ```

## Network Connectivity

### NetworkPolicy Blocking Traffic

**Symptoms**:

- Metrics not accessible from Prometheus
- Health checks failing
- Cannot communicate with Cloudflare API

**Diagnosis**:

```bash
# Check NetworkPolicy rules
kubectl get networkpolicy --namespace cloudflare-tunnel-system

# Test connectivity from debug pod
kubectl run debug --rm -it --image=nicolaka/netshoot \
  --namespace cloudflare-tunnel-system -- bash

# Inside debug pod:
curl http://cloudflare-tunnel-gateway-controller:8080/metrics
curl http://cloudflare-tunnel-gateway-controller:8081/healthz
curl --head https://api.cloudflare.com
```

### DNS Resolution Issues

**Symptoms**:

- Cannot resolve Cloudflare API endpoints
- Errors: `no such host` or `dial tcp: lookup failed`

**Diagnosis**:

```bash
kubectl exec --namespace cloudflare-tunnel-system POD_NAME -- \
  cat /etc/resolv.conf

kubectl exec --namespace cloudflare-tunnel-system POD_NAME -- \
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
```

## Gateway API Resources

### Gateway Not Ready

**Diagnosis**:

```bash
kubectl get gateway --all-namespaces
kubectl describe gateway my-gateway --namespace my-namespace

kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller | grep -i gateway
```

**Common causes**:

| Issue | Solution |
|-------|----------|
| GatewayClass not found | Set `gatewayClass.create: true` in Helm values |
| Wrong controller name | Check `gatewayClassName` matches chart configuration |
| Service not found | Verify backend Services exist |

### HTTPRoute Not Attached

**Diagnosis**:

```bash
kubectl describe httproute my-route --namespace my-namespace
```

**Common causes**:

- Namespace mismatch (use ReferenceGrant for cross-namespace)
- Invalid hostname patterns
- Backend service not found

### Status Not Updating

**Problem**: Gateway/HTTPRoute status conditions not updating

**Diagnosis**:

```bash
kubectl auth can-i update gateways/status \
  --as=system:serviceaccount:cloudflare-tunnel-system:cloudflare-tunnel-gateway-controller
```

**Solution**: Ensure ClusterRole has status subresource permissions

## AmneziaWG Sidecar Issues

### AWG Interface Creation Failures

**Symptoms**:

- Container crash with `Operation not permitted`
- Interface conflicts: `device already exists`

**Diagnosis**:

```bash
kubectl get deployment --namespace cloudflare-tunnel-system \
  cloudflare-tunnel-gateway-controller --output yaml | grep -A 10 securityContext

kubectl logs --namespace cloudflare-tunnel-system POD_NAME --container amneziawg
```

**Solutions**:

1. AWG requires `NET_ADMIN` capability (chart handles this automatically)

2. Use different interface prefixes for conflicts:

    ```yaml
    awg:
      interfacePrefix: "awg-prod"
    ```

### AWG DNS Overwrites Cluster DNS

**Problem**: cloudflared cannot resolve internal Kubernetes services

**Symptoms**:

- `/etc/resolv.conf` shows VPN DNS instead of CoreDNS
- Logs show: `no such host` for internal service names

**Solution**: This is handled automatically in chart version 0.2.x+. For older
versions, remove `DNS = ...` line from AWG config file.

**Verification**:

```bash
kubectl exec --namespace cloudflare-tunnel-system POD_NAME \
  --container cloudflared -- cat /etc/resolv.conf
# Should show CoreDNS IP (e.g., 10.96.0.10), not 1.1.1.1
```

## Performance Issues

### High Memory Usage

**Symptoms**:

- Pods OOMKilled
- Memory usage growing over time

**Diagnosis**:

```bash
kubectl top pod --namespace cloudflare-tunnel-system
```

**Solution**:

```yaml
resources:
  limits:
    memory: 512Mi  # Increase from default
  requests:
    memory: 256Mi
```

### Slow Reconciliation

**Problem**: Changes to Gateway/HTTPRoute take long to apply

**Diagnosis**:

```bash
kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller | grep -i reconcil
```

**Solutions**:

1. Check Cloudflare API rate limits in logs
2. Verify network latency to Cloudflare API
3. Ensure sufficient resources (CPU not throttled)

## Debug Logging

Enable debug logging for detailed diagnostics:

```yaml
controller:
  logLevel: "debug"
  logFormat: "json"
```

```bash
helm upgrade cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --values values.yaml \
  --namespace cloudflare-tunnel-system

kubectl logs --follow --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller
```

## Collecting Diagnostic Information

```bash
# Pod status and events
kubectl get pods --namespace cloudflare-tunnel-system --output wide
kubectl describe pod --namespace cloudflare-tunnel-system POD_NAME

# Recent logs
kubectl logs --tail=100 --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

# Resource usage
kubectl top pod --namespace cloudflare-tunnel-system

# Gateway API resources
kubectl get gatewayclasses,gateways,httproutes --all-namespaces
```

## Reporting Issues

When reporting issues, include:

1. Helm chart version: `helm list --namespace cloudflare-tunnel-system`
2. Kubernetes version: `kubectl version`
3. Cloud provider and CNI plugin
4. Relevant pod logs (sanitize secrets!)
5. Gateway/HTTPRoute manifests (sanitize sensitive data)

Report issues at: <https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues>
