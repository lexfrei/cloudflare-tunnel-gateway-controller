# Upgrading from v2.x to v3.0

v3 collapses the two data plane modes that the v1/v2 chart supported (a separately-managed cloudflared deployment plus an opt-in L7 proxy) into a single unified data plane: the L7 proxy binary embeds cloudflared transport in-process and is the only path tunnel traffic takes. This page lists the breaking changes and the steps to migrate.

## What changed

- **The chart no longer renders `proxy.enabled: false`.** The proxy Deployment, Services, NetworkPolicy and ServiceMonitor are always rendered; `proxy.tunnelTokenSecretRef.name` is now mandatory — the chart's template-level `{{ required "..." }}` check in `templates/deployment-proxy.yaml` fails the install when the value is empty.
- **The controller no longer manages a separate cloudflared deployment.** All Helm SDK code paths inside the controller are gone — there is no longer an in-cluster Helm release named `cfd-<gateway>` for each Gateway. cloudflared transport now runs inside the proxy pod, configured via the chart's `proxy.tunnelTokenSecretRef`.
- **The GatewayClassConfig CRD is slimmer.** `tunnelTokenSecretRef` and the entire `cloudflared` block (`enabled`, `replicas`, `namespace`, `protocol`, `awg`, `livenessProbe`) have been removed from the spec. Proxy-side configuration moves to chart values.
- **`--proxy-endpoints` is required at controller startup.** The bootstrap fails fast with a clear error if the flag is empty.

## Migration steps

0. **Apply the v3 CRD BEFORE `helm upgrade`.** Helm 3's `crds/` directory installs CRDs only on the first `helm install`; `helm upgrade` deliberately never touches them. Without this step the v2 CRD's CEL validation (`tunnelTokenSecretRef is required when cloudflared.enabled is true`) fails the v3 template's stripped GatewayClassConfig, and the upgrade aborts with a confusing `admission webhook denied the request` error.

    ```bash
    # Apply the v3 CRD shipped with this release (replace <tag> with the v3.x.y
    # version you're upgrading to).
    kubectl apply --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/<tag>/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayclassconfigs.yaml
    ```

    The v3 CRD drops the CEL rule that mentioned `cloudflared.enabled` and `tunnelTokenSecretRef`; the rendered v3 `GatewayClassConfig` then validates cleanly.

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

    !!! tip "Use `--reset-then-reuse-values` on `helm upgrade`"

        The v3 chart introduces a required value (`proxy.tunnelTokenSecretRef.name`) that the v2 defaults didn't carry. `helm upgrade --reuse-values` only re-applies the user overrides from the previous release and drops new chart defaults — so the install fails with the chart's `required` error. Pass `--reset-then-reuse-values` (Helm 3.14+) so new defaults merge under your overrides.

3. **Clean up the legacy in-cluster cloudflared releases**, if any. The controller no longer reconciles them, but a leftover `cfd-<gateway>` Helm release will keep an orphaned cloudflared Deployment running. Discover them and uninstall:

    ```bash
    # Legacy releases were created in the controller's own namespace
    # (typically cloudflare-tunnel-system), one per managed Gateway.
    helm list --all-namespaces --filter '^cfd-'

    # Then for each one:
    helm uninstall <release-name> --namespace <namespace>
    ```

4. **Make sure the controller deployment passes `--proxy-endpoints`.** The chart wires this unconditionally — only out-of-tree deployments that ran the controller binary directly need to add the flag. The expected value points at the proxy's headless Service (`http://<fullname>-proxy-headless.<namespace>.svc.<cluster-domain>:<proxy.configAPIPort>/config`), where `<fullname>` is the Helm release fullname (typically `<release>-cloudflare-tunnel-gateway-controller`, or just `<release>` when the release name already contains the chart name). To read the exact name your release rendered, run `helm get manifest <release> | grep -m1 'proxy-headless'`.

5. **No data migration is required for CRs.** The Kubernetes API server prunes unknown fields when you apply the new CRD schema (the v3 CRD does not set `x-kubernetes-preserve-unknown-fields: true`, so apiextensions/v1's default pruning applies), so existing GatewayClassConfig resources continue to work — the removed `cloudflared` and `tunnelTokenSecretRef` fields are silently dropped on next read/write.

6. **Legacy finalizer cleanup is automatic on delete.** The v2 controller attached a `cloudflare-tunnel.gateway.networking.k8s.io/cloudflared` finalizer to every Gateway it reconciled. The v3 controller does not strip the finalizer from live Gateways — it sits there harmlessly until the Gateway is actually deleted, at which point the deletion path removes it on the first reconcile and the Gateway proceeds with normal termination.

    If you want to strip it proactively without deleting the Gateway, use a **conditional JSON-patch** that aborts if the finalizer is not at the expected index — never a bare indexed `remove` that can silently delete the wrong finalizer if the list is reordered between your look-up and the patch:

    ```bash
    # Inspect the current finalizer list first.
    kubectl get gateway <name> -n <ns> -o json | jq '.metadata.finalizers'

    # Replace 0 with the index returned above. The `test` op makes the
    # patch fail loudly if the index does not still point at the legacy
    # finalizer, so the `remove` op cannot delete the wrong entry.
    kubectl patch gateway <name> -n <ns> --type=json -p='[
      {"op":"test","path":"/metadata/finalizers/0","value":"cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"},
      {"op":"remove","path":"/metadata/finalizers/0"}
    ]'
    ```

    Alternatively, when the Gateway has no other finalizers (or you want to drop them all), use a merge patch that rewrites the list without the legacy entry — caveat: this replaces the entire list, so include any other finalizers you want to preserve:

    ```bash
    kubectl patch gateway <name> -n <ns> --type=merge \
      -p '{"metadata":{"finalizers":[]}}'
    ```

## GRPCRoute routing

v2 (default) routed gRPC traffic via cloudflared's native ingress. v3 collapses everything to the L7 proxy. Early v3 builds had no gRPC matcher and returned `404 no matching route`; current v3 serves GRPCRoute through the proxy — gRPC service/method matches map onto `/{service}/{method}` path rules and the upstream hop is h2c. No migration is needed for `GRPCRoute` resources; they route as before. See [GRPCRoute](../gateway-api/grpcroute.md).

## AmneziaWG sidecar is gone

The AmneziaWG sidecar was a feature of the legacy cloudflared-managed-by-controller path: the controller's Helm SDK render of cloudflared wired in an AWG sidecar that intercepted the cloudflared egress. v3 has no separate cloudflared deployment, no Helm SDK render, and no sidecar slot on the proxy pod, so AWG is no longer offered as a built-in option. If you relied on AWG to obfuscate the tunnel transport, stay on the v2.x chart line until upstream re-introduces an equivalent.

## Why this is a breaking change

The v2 chart supported two independent ways to terminate Cloudflare Tunnel traffic, and both were on by default. The L7 proxy was the path that actually receives Gateway API features (regex matching, filters, URL rewrites, CORS), so leaving the legacy cloudflared-only mode in place mostly led to silent feature gaps when users discovered their HTTPRoute filters were not being honoured. Collapsing to a single data plane removes the foot-gun and lets the controller's status reporting match what the data plane is actually doing.

## Staying on v2

The v2.x chart line continues to receive critical fixes for a period. If you cannot migrate yet, pin the chart version and watch the v2 release notes for the cut-off date.
