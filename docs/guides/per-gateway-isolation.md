# Per-Gateway Isolation

By default every Gateway of the class shares one chart-deployed proxy pool and one Cloudflare Tunnel. This guide covers the opt-in hard-isolation mode: a DEDICATED data plane — its own proxy Deployment and its own tunnel — rendered and reconciled by the controller for a single Gateway.

Use it when admission-level scoping (see [Multi-Tenancy](multi-tenancy.md)) is not enough: tenants that must not share a process, a tunnel identity, or a failure domain.

## What you get

- A dedicated proxy Deployment in the Gateway's namespace, running the same proxy image as the shared plane (chart-wired via `--proxy-image`, overridable per Gateway).
- A dedicated Cloudflare Tunnel: the Gateway's routes are written to ITS tunnel's ingress document and pushed to ITS proxy pods only. Routes of other Gateways never reach this data plane — and this Gateway's routes never reach theirs.
- Independent lifecycle: rendered resources are controller-owned (deleted with the Gateway, healed on drift), connector draining on shutdown, optional autoscaling on the proxy's in-flight gauge.
- A per-Gateway config-push credential: the controller authenticates config pushes to this plane with the Gateway's own token, never the shared plane's.

## Opting in

1. Create a Cloudflare Tunnel for the Gateway (one tunnel per isolated Gateway) and store its connector token in the Gateway's namespace:

    ```bash
    kubectl --context <ctx> --namespace tenant-a create secret generic edge-tunnel-token \
      --from-literal=tunnel-token=<connector-token>
    ```

2. Create a `GatewayConfig` next to the Gateway:

    ```yaml
    apiVersion: cf.k8s.lex.la/v1alpha1
    kind: GatewayConfig
    metadata:
      name: edge-config
      namespace: tenant-a
    spec:
      tunnelTokenSecretRef:
        name: edge-tunnel-token
      replicas: 2
    ```

3. Reference it from the Gateway:

    ```yaml
    apiVersion: gateway.networking.k8s.io/v1
    kind: Gateway
    metadata:
      name: edge
      namespace: tenant-a
    spec:
      gatewayClassName: cloudflare-tunnel
      infrastructure:
        parametersRef:
          group: cf.k8s.lex.la
          kind: GatewayConfig
          name: edge-config
      listeners:
        - name: https
          port: 443
          protocol: HTTPS
    ```

The controller renders `cf-proxy-edge` (Deployment) and `cf-proxy-edge-config` (headless Service) in `tenant-a`, parses the tunnel identity from the connector token (there is deliberately no separate `tunnelID` field — it cannot drift from the token), and starts syncing the Gateway's routes to that tunnel. The Gateway's status address becomes `<tunnel-id>.cfargotunnel.com`, and `Programmed` turns `True` only once the rendered Deployment has ready replicas — that is, registered tunnel connectors.

A Gateway without `infrastructure.parametersRef` keeps the shared data plane, unchanged. Removing the ref later deletes the rendered resources (only when actually owned by the Gateway) and returns the Gateway to the shared plane.

## GatewayConfig reference

| Field | Required | Meaning |
| --- | --- | --- |
| `tunnelTokenSecretRef` | yes | Connector-token Secret in the SAME namespace (key `tunnel-token` by default). Token rotation rolls the proxy pods automatically. |
| `cloudflareCredentialsSecretRef` | no | API-token override for this Gateway's tunnel-document writes, from a Secret in the SAME namespace (key `api-token` by default); defaults to the GatewayClass → GatewayClassConfig credentials. |
| `authTokenSecretRef` | no | Bearer token (key `auth-token`) protecting this plane's config API; the controller pushes with it. |
| `replicas` | no | Fixed replica count; default 2 (the HA floor — one connector pod is a tunnel availability hazard), max 100. Mutually exclusive with `autoscaling`. |
| `autoscaling` | no | Renders a HorizontalPodAutoscaler — see below. `minReplicas`/`maxReplicas` cap at 100. |
| `resources` | no | Proxy container resources; chart-parity defaults when unset. |
| `image` | no | Proxy image override; defaults to the release's proxy image. |

All Secret references are namespace-local by construction — a Gateway cannot point at another tenant's credentials.

`Gateway.spec.infrastructure.labels` and `.annotations` propagate to the rendered resources and the pod template; changing them rolls the pods.

## Autoscaling

```yaml
spec:
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetInflightPerPod: 50
```

The rendered `autoscaling/v2` HPA scales the proxy Deployment on `cftunnel_proxy_requests_in_flight` as a Pods-type custom metric — concurrency is the saturation signal for an I/O-bound L7 hop, not CPU. Serving Pods metrics to the HPA requires a metrics adapter (prometheus-adapter or KEDA) exposing the gauge through the custom-metrics API; without one the HPA reports `FailedGetPodsMetric` and holds `minReplicas` — visible degradation, never silent. See [Metrics & Alerting](../operations/metrics.md) for adapter examples.

Do not pair a VerticalPodAutoscaler in apply mode with these Deployments: applying VPA recommendations restarts pods, which drops tunnel connectors. Recommendation mode is fine.

## Sharing a tunnel (supported, but not isolation)

If a per-Gateway token points at the SAME tunnel as the shared plane (or another Gateway), the controller merges their ingress documents into one write and pushes the UNION of that tunnel's routes to every data plane on it — Cloudflare load-balances a tunnel's requests across all its connectors, so every connector must know every route. This keeps a shared-tunnel setup working (useful for migrations), but the isolation properties only hold for distinct tunnels.

## Securing a tenant data plane

A tenant data plane is locked down by default on two axes:

- **The config API is authenticated.** When a GatewayConfig declares no `authTokenSecretRef`, the controller generates a random bearer-token Secret (`cf-proxy-<gateway>-auth`, controller-owned, never rotated) and wires the proxy to it, so a tenant plane is never exposed unauthenticated. `authTokenSecretRef` is a bring-your-own-token OVERRIDE for operators who want to manage the token themselves (external secret stores, scheduled rotation). The proxy reads the token from a `SecretKeyRef` env var at start, so the pod template hashes the resolved token: rotating the bring-your-own Secret rolls the proxy pods automatically (same as `tunnelTokenSecretRef`), and the controller re-authenticates its config pushes with the new token.
- **The config API is network-restricted.** The controller renders a NetworkPolicy per data plane that admits the config API port (8081) only from the **controller's** namespace (not the tenant's), so neither the config API nor the `/metrics` it co-serves is reachable from arbitrary pods — including other pods in the Gateway's own namespace. The proxy's data port (8080) takes no in-cluster ingress at all — traffic arrives through the outbound tunnel. Because `/metrics` shares the locked port, a policy that admits only the controller silently breaks Prometheus scraping and therefore the rendered HPA (it reports `FailedGetPodsMetric` and holds `minReplicas`); set `proxy.networkPolicy.monitoringNamespaceSelector` in the chart to additionally admit your monitoring namespace. Where the CNI does not enforce NetworkPolicy this layer is a no-op (defense in depth, not a guarantee).

Also note the RBAC equivalence: `create` on `GatewayConfig` (plus a Gateway referencing it) lets a user run an arbitrary image via `spec.image` under the namespace's default ServiceAccount — see the [security reference](../reference/security.md).

## Operational notes

- **Events:** the controller emits `ProxyProvisioned` (Normal) on the Gateway when the data plane is rendered, and `RenderFailed` (Warning) when rendering cannot proceed (apply failures) — `kubectl describe gateway` shows both.
- **No proxy image configured:** if neither `GatewayConfig.spec.image` nor the controller's `--proxy-image` default is set, the data plane cannot be rendered. The Gateway surfaces `Accepted=False` with reason `InvalidParameters` and a message naming the missing image (a persistent condition, not just a transient Event) — set one of the two and the Gateway recovers on the next reconcile.
- **Drain:** on pod shutdown the proxy unregisters its connectors from the edge and gives in-flight requests a grace period before exiting; the rendered `terminationGracePeriodSeconds` covers the window.
- **RBAC:** rendering requires cluster-wide write on Deployments/Services/HPAs (Gateways live in arbitrary namespaces); see the [security reference](../reference/security.md) for the exact rules and ownership guards.
- **Failure containment:** a tunnel-sync failure for one Gateway's tunnel marks only THAT Gateway's routes Pending; other tenants' route statuses are untouched.
- **Post-render breakage:** if the GatewayConfig stops resolving AFTER a healthy render (token Secret deleted, ref broken), new route changes fail closed — they are not programmed anywhere — but the already-running data plane keeps serving its LAST pushed config until the configuration resolves again or the Gateway is deleted. The Gateway surfaces `InvalidParameters`, and a route bound only to the broken Gateway reports `Accepted=False` on that parent (it is served nowhere) — a route accepted on a healthy parent too keeps `Accepted=True` there.
- **Not rendered:** per-Gateway PodDisruptionBudget and ServiceMonitor. The shared plane's ServiceMonitor does not select rendered pods; scrape them with a PodMonitor on the `cf.k8s.lex.la/gateway` label if needed (and admit the monitoring namespace via `proxy.networkPolicy.monitoringNamespaceSelector`, since the rendered NetworkPolicy locks the metrics port).
- **Cloudflare-side residue on teardown:** deleting the Gateway (or removing `infrastructure.parametersRef`) garbage-collects the in-cluster resources, but the LAST ingress document written to the dedicated Cloudflare Tunnel is left in place. It is inert — the connectors are gone, so nothing serves it — but it is not erased from the Cloudflare API. Delete or repurpose the tunnel on the Cloudflare side if you need the account state clean.
- **API-credential rotation is eventually consistent:** rotating the `cloudflareCredentialsSecretRef` override (the controller-side API token, distinct from the connector token) is not watched directly, so the new token is picked up on the NEXT sync of a route bound to the Gateway rather than immediately. To force it, trigger any route change on the Gateway (or restart the controller). Connector-token (`tunnelTokenSecretRef`) rotation IS immediate — it re-renders the proxy Deployment.
