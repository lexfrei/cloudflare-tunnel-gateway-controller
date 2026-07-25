# Troubleshooting

This guide helps diagnose and resolve common issues with the Cloudflare Tunnel Gateway Controller.

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

1. Empty `tunnelID`:

    ```text
    Error: execution error at (...gatewayclassconfig.yaml:20:16): gatewayClassConfig.tunnelID is required
    ```

    **Solution**: Provide a valid Tunnel ID from Cloudflare Zero Trust Dashboard

2. Invalid characters in `tunnelID`:

    ```text
    Error: at '/gatewayClassConfig/tunnelID': pattern mismatch
    ```

    **Solution**: Use a valid UUID from the Cloudflare Zero Trust Dashboard (format: `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` with lowercase hex digits a-f and 0-9 only)

3. Missing API credentials:

    ```text
    Error: execution error at (...gatewayclassconfig.yaml:10:12): gatewayClassConfig.cloudflareCredentialsSecretRef.name is required
    ```

    **Solution**: Set `gatewayClassConfig.cloudflareCredentialsSecretRef.name` to the name of a Secret containing an `api-token` key

4. Missing proxy tunnel token:

    ```text
    Error: execution error at (...deployment-proxy.yaml:41:18): proxy.tunnelTokenSecretRef.name is required
    ```

    **Solution**: Set `proxy.tunnelTokenSecretRef.name` to the name of a Secret containing a `tunnel-token` key (see Getting Started / Prerequisites). This field is required on every install, before any `gatewayClassConfig.*` value.

**Verification**:

```bash
# Validate values before installation
helm lint charts/cloudflare-tunnel-gateway-controller --values my-values.yaml

# Dry-run installation
helm install --dry-run --debug my-release \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
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

### Config API Auth Secret Missing or Broken

**Symptoms**:

- Controller pod in `CrashLoopBackOff` immediately after startup, before it reconciles anything
- Every Gateway managed by this controller loses reconciliation, not just one

**Diagnosis**:

```bash
kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller
```

```text
error: failed to run controller: resolving shared-proxy auth secret: cloudflare-tunnel-system/cloudflare-tunnel-gateway-controller-proxy-auth-token key "auth-token": shared-proxy auth secret exists but has no usable value at the expected key
```

**Cause**: a Secret already exists at the name the controller expects for the shared proxy's config-API bearer token, but its `auth-token` key is missing or empty. Most often this is a hand-created Secret with a typo'd key name. The controller resolves this token once at startup, before wiring any reconciler, and fails closed rather than start with an unusable token: the config API must never be left unauthenticated. Unlike a broken per-Gateway auth Secret, which only stops that one Gateway's data plane, this stops the controller process entirely, because the resolution happens before any Gateway is reconciled.

The controller cannot repair this itself. Its RBAC on Secrets grants `create` but not `update`.

!!! danger "A present-but-empty key can leave the proxy silently unauthenticated"

    Kubernetes only refuses to start a container over a `secretKeyRef` when the key is *absent*. A key that exists with an empty value resolves fine, so if the proxy pod started against this Secret before you noticed the problem, it is running with `PROXY_AUTH_TOKEN=""` right now — and the proxy treats an empty token as "no auth configured", accepting `PUT /config` from anyone. The controller crash-looping hides this: it looks like nothing is working, but the proxy's config API can be wide open the whole time. Deleting the Secret and restarting only the controller does not fix this proxy pod — it already has the empty value baked into its process environment and won't reread the Secret on its own. Restart the proxy too.

**Solution**: delete the broken Secret and restart both the controller and the proxy, so neither is left running with an empty or stale token.

```bash
kubectl delete secret cloudflare-tunnel-gateway-controller-proxy-auth-token \
  --namespace cloudflare-tunnel-system

kubectl rollout restart deployment/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system

kubectl rollout restart deployment/cloudflare-tunnel-gateway-controller-proxy \
  --namespace cloudflare-tunnel-system
```

If `proxy.authTokenSecretRef.name` is set to a Secret you manage yourself, the controller never creates or repairs it; fix the key in that Secret instead, then restart the proxy the same way so it picks up the corrected value.

### Proxy Pod Stuck NotReady After a Restart

**Symptoms**:

- A proxy pod is `Running` but `0/1 Ready` for longer than expected after a restart or node reboot, with a low or zero `RESTARTS` count in `kubectl get pods` — this is the tell that distinguishes the new behavior from the old crash loop an operator might be expecting
- Logs repeat `tunnel bootstrap dial failed, retrying`, one line per attempt

**Cause**: this is expected, not a bug. The proxy retries the tunnel's bootstrap dial instead of exiting on a transient failure — cluster DNS not yet reachable while the CNI is still wiring pods up after a reboot, or the Cloudflare edge briefly unreachable. Each retry waits a random duration between 0 and an exponentially growing cap (2s up to 30s, jittered so that replicas which failed at the same instant do not all retry in lockstep), so the logged `backoff` value moves around rather than climbing in a straight line. `/readyz` stays false for the whole retry window; the pod does not crash-loop.

**Diagnosis**: a proxy that is retrying, not wedged, keeps emitting a new `tunnel bootstrap dial failed, retrying` log line on every attempt — an `attempt` count that keeps climbing is the pod actively working the problem, not stuck. Follow the logs to watch it happen:

```bash
kubectl logs --namespace cloudflare-tunnel-system \
  --selector app.kubernetes.io/component=proxy --follow
```

**Solution**: if the retry log line keeps repeating well past the time the underlying network issue should have cleared (DNS, egress to the Cloudflare edge), check the error text on each attempt:

- A DNS/dial/edge-unreachable error clears on its own once connectivity is restored — no action needed.
- The same error persisting for many minutes with no change usually means the tunnel token itself is invalid or was revoked in the Cloudflare Zero Trust Dashboard. A malformed token (fails to decode) makes the pod exit immediately instead of retrying; a well-formed but rejected token retries indefinitely, because the proxy cannot reliably distinguish "the edge rejected this token" from "the edge is temporarily unreachable" from the error alone. Regenerate the connector token and update `proxy.tunnelTokenSecretRef` if this is the case.

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
   - Account > Cloudflare Tunnel > Edit

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
| Wrong controller name | Check GatewayClass `spec.controllerName` matches `controller.controllerName` in chart values |
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
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
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
