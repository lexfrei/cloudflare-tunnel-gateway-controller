# Upgrading from v3.0 to v3.1

v3.1 hardens multi-tenant isolation. There is no CRD migration and no required values change — existing `GatewayClassConfig` and chart values keep working. But four behaviours change in ways existing automation can observe, and one of them (the data-plane NetworkPolicy now on by default) can break an existing Prometheus scrape if your monitoring namespace is not admitted. Read the four notes below; only the metrics/NetworkPolicy one needs action, and only for some setups.

## What changed

### 1. Route `Accepted` Reason precedence

A route that is rejected at binding (for example `HostnameNotPermitted`, or no matching parent) is now reported with its specific, actionable Reason even during a tunnel outage. Previously a transient sync failure could mask the binding rejection with a generic `Pending`. A binding rejection is permanent — the route is never programmed regardless of tunnel health — so its Reason now outranks `Pending`.

If you script against the `Accepted` condition Reason during outages, expect the more specific Reason (e.g. `HostnameNotPermitted`) instead of `Pending` on routes that were already failing to bind. Routes that bind cleanly and only fail to sync still report `Pending` as before. See the route-status table in the [CRD reference](../reference/crd-reference.md).

### 2. New `RouteShadowed` condition and Warning Event

When a route's `(hostname, match)` pair is exactly claimed by a higher-precedence route, the losing route now carries a dedicated `cf.k8s.lex.la/RouteShadowed` condition (its `Accepted` stays `True` — same-hostname routes merge legally per the Gateway API) and a mirrored `RouteShadowed` Warning Event. Monitoring that alerts on Warning Events or on unknown condition types will start firing on collisions that were previously silent. The condition clears automatically when the collision is resolved. See [Detecting collisions](../guides/multi-tenancy.md#detecting-collisions).

### 3. Proxy termination grace period

The proxy pod's `terminationGracePeriodSeconds` is now derived as `proxy.gracePeriodSeconds + 15` (so 45 by default, up from a hard-coded 30) and the proxy receives a `PROXY_GRACE_PERIOD` env var carrying `proxy.gracePeriodSeconds` (default `30s`). On shutdown the proxy unregisters its connectors from the edge and drains in-flight requests for that window before exiting; the extra 15s of pod headroom keeps Kubernetes from killing the pod mid-drain. `proxy.gracePeriodSeconds` MUST stay below the pod grace period — the chart enforces that by computing the pod value from it.

### 4. Proxy metrics on by default, behind a default-on NetworkPolicy

Two coupled changes:

- `proxy.metrics.enabled` now defaults to `true`. The proxy's `/metrics` endpoint (config API port 8081) now exposes request-level series (`cftunnel_proxy_*`: in-flight, duration, status classes, bytes, backend errors) in addition to the embedded cloudflared connector metrics it already served. This is additive — new series appear, nothing is removed. The proxy ServiceMonitor stays opt-in (`serviceMonitor.enabled: false` by default).
- `proxy.networkPolicy.enabled` now defaults to `true`. The chart renders an ingress-only NetworkPolicy that admits the config API port (which also carries `/metrics`) only from the controller's own namespace. The proxy data port (8080) takes no in-cluster ingress — tunnel traffic arrives outbound. The controller also renders an equivalent NetworkPolicy for each per-Gateway data plane.

The second change is the one that can break an existing setup — see below.

## The change that can break: Prometheus scraping

If you already scrape the proxy `/metrics` (you set `serviceMonitor.enabled: true` on v3.0, or wired a manual scrape) from a namespace other than the controller's, the new default-on NetworkPolicy will block that scrape after upgrade, because the policy admits port 8081 only from the controller namespace.

Re-admit your monitoring namespace:

```yaml
proxy:
  networkPolicy:
    # Shared proxy plane: list the namespaces allowed to reach the config API /
    # metrics port. Setting ingress.from REPLACES the controller-namespace
    # default, so include the controller namespace too if the controller still
    # pushes config from there.
    ingress:
      from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: cloudflare-tunnel-system
    # Per-Gateway (tenant) data planes the controller renders: a label selector
    # for namespaces additionally admitted to their config API / metrics port.
    # The controller namespace is always admitted; this only adds to it.
    monitoringNamespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: monitoring
```

If you do not run a NetworkPolicy-enforcing CNI, the policy is a no-op and scraping is unaffected — but it is then also not providing the isolation, so treat it as defense in depth, not a guarantee. To keep the v3.0 behaviour (no data-plane NetworkPolicy at all), set `proxy.networkPolicy.enabled: false`.

## Verify after upgrade

- **Kubelet probes.** The proxy's startup/liveness/readiness probes hit the config API port (8081). The rendered NetworkPolicy admits 8081 from the controller namespace only; kubelet probe traffic originates from the node, so it relies on the CNI allowing host→pod traffic (most do). If proxy pods go `NotReady` after upgrade on a strict CNI, that allowance is the thing to check.
- **Controller → proxy config push.** The controller pushes config to the proxy from its own namespace, which the default policy admits; verify routes still program after upgrade.

## No CRD or values migration

No `GatewayClassConfig` change is required and no values are removed — the defaults flip (`proxy.metrics.enabled`, `proxy.networkPolicy.enabled`) but every key keeps its v3.0 meaning. Pin `proxy.metrics.enabled: false` and/or `proxy.networkPolicy.enabled: false` to retain the exact v3.0 data-plane shape.
