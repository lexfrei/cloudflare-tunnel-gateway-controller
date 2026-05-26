# Upgrading from v2.x to v3.0

v3 collapses the two data plane modes that the v1/v2 chart supported (a separately-managed cloudflared deployment plus an opt-in L7 proxy) into a single unified data plane: the L7 proxy binary embeds cloudflared transport in-process and is the only path tunnel traffic takes. This page lists the breaking changes and the steps to migrate.

## What changed

- **The chart no longer renders `proxy.enabled: false`.** The proxy Deployment, Services, NetworkPolicy and ServiceMonitor are always rendered; `proxy.tunnelTokenSecretRef.name` is now mandatory (the schema rejects empty values, and the template's `required` check fires on install).
- **The controller no longer manages a separate cloudflared deployment.** All Helm SDK code paths inside the controller are gone — there is no longer an in-cluster Helm release named `cfd-<gateway>` for each Gateway. cloudflared transport now runs inside the proxy pod, configured via the chart's `proxy.tunnelTokenSecretRef`.
- **The GatewayClassConfig CRD is slimmer.** `tunnelTokenSecretRef` and the entire `cloudflared` block (`enabled`, `replicas`, `namespace`, `protocol`, `awg`, `livenessProbe`) have been removed from the spec. Proxy-side configuration moves to chart values.
- **`--proxy-endpoints` is required at controller startup.** The bootstrap fails fast with a clear error if the flag is empty.

## Migration steps

1. **Replace `gatewayClassConfig.cloudflared.*` and `gatewayClassConfig.tunnelTokenSecretRef` with proxy-side equivalents.** Move the tunnel token Secret reference from the CRD into the chart values:

    ```yaml
    # before (v2)
    gatewayClassConfig:
      tunnelTokenSecretRef:
        name: cloudflare-tunnel-token
      cloudflared:
        enabled: true
        replicas: 2

    # after (v3)
    gatewayClassConfig:
      # spec now only carries cloudflareCredentialsSecretRef, accountId, tunnelID

    proxy:
      tunnelTokenSecretRef:
        name: cloudflare-tunnel-token
      replicas: 2
    ```

2. **Drop `proxy.enabled: false` if you ever set it.** v2 users who ran the controller with the L7 proxy disabled need to set `proxy.tunnelTokenSecretRef.name` before upgrading, otherwise the chart install fails on the required check. The proxy is the only data plane in v3.

3. **Clean up the legacy in-cluster cloudflared releases**, if any. The controller no longer reconciles them, but a leftover `cfd-<gateway>` Helm release will keep an orphaned cloudflared Deployment running. Discover them and uninstall:

    ```bash
    # Legacy releases were created in the controller's own namespace
    # (typically cloudflare-tunnel-system), one per managed Gateway.
    helm list --all-namespaces --filter '^cfd-'

    # Then for each one:
    helm uninstall <release-name> --namespace <namespace>
    ```

4. **Make sure the controller deployment passes `--proxy-endpoints`.** The chart wires this unconditionally — only out-of-tree deployments that ran the controller binary directly need to add the flag. The expected value points at the proxy's headless Service (`http://<release>-proxy-headless.<namespace>.svc.<cluster-domain>:<proxy.configAPIPort>/config`).

5. **No data migration is required for CRs.** The Kubernetes API server strips unknown fields when you apply the new CRD schema, so existing GatewayClassConfig resources continue to work — the removed fields are simply ignored. (`spec.additionalProperties` is deliberately left unset on the CRD schema so v2 CRs apply without modification.)

6. **Legacy finalizer cleanup is automatic.** The v2 controller attached a `cloudflare-tunnel.gateway.networking.k8s.io/cloudflared` finalizer to every Gateway it reconciled. The v3 controller strips this finalizer on first reconcile when the Gateway is being deleted. No manual `kubectl patch` is required — but if you do `kubectl get gateway -o yaml` immediately after upgrade, you will still see the finalizer until the next reconcile event.

## AmneziaWG sidecar is gone

The AmneziaWG sidecar was a feature of the legacy cloudflared-managed-by-controller path: the controller's Helm SDK render of cloudflared wired in an AWG sidecar that intercepted the cloudflared egress. v3 has no separate cloudflared deployment, no Helm SDK render, and no sidecar slot on the proxy pod, so AWG is no longer offered as a built-in option. If you relied on AWG to obfuscate the tunnel transport, stay on the v2.x chart line until upstream re-introduces an equivalent.

## Why this is a breaking change

The v2 chart supported two independent ways to terminate Cloudflare Tunnel traffic, and both were on by default. The L7 proxy was the path that actually receives Gateway API features (regex matching, filters, URL rewrites, CORS), so leaving the legacy cloudflared-only mode in place mostly led to silent feature gaps when users discovered their HTTPRoute filters were not being honoured. Collapsing to a single data plane removes the foot-gun and lets the controller's status reporting match what the data plane is actually doing.

## Staying on v2

The v2.x chart line continues to receive critical fixes for a period. If you cannot migrate yet, pin the chart version and watch the v2 release notes for the cut-off date.
