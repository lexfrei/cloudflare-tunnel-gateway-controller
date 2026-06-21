# Upgrading from v3.0 to v3.1

v3.1 hardens multi-tenant isolation. There is no CRD migration and no required values change — existing `GatewayClassConfig` and chart values keep working. But four behaviours change in ways existing automation can observe, and one of them — the data-plane NetworkPolicy now on by default — can break two things on a NetworkPolicy-enforcing CNI: an existing Prometheus scrape from an unadmitted namespace, and, more seriously, proxy pod readiness if the CNI also enforces host→pod (kubelet) traffic. Read the four notes below, then the two "change that can break" sections; only the metrics/NetworkPolicy one needs action, and only on some setups.

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
    # Shared proxy plane: EXTRA namespaces allowed to reach the config API /
    # metrics port. ingress.from is ADDED to the controller namespace (always
    # admitted, so a config push is never locked out) — list only your
    # monitoring namespace here.
    ingress:
      from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
    # Per-Gateway (tenant) data planes the controller renders: a label selector
    # for namespaces additionally admitted to their config API / metrics port.
    # The controller namespace is always admitted; this only adds to it.
    monitoringNamespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: monitoring
```

If you do not run a NetworkPolicy-enforcing CNI, the policy is a no-op and scraping is unaffected — but it is then also not providing the isolation, so treat it as defense in depth, not a guarantee. To keep the v3.0 behaviour (no data-plane NetworkPolicy at all), set `proxy.networkPolicy.enabled: false` — that one switch gates BOTH the chart's shared-proxy policy AND the per-Gateway policies the controller renders (it forwards the value as the controller's `--render-network-policy` flag, which deletes any policy it previously rendered).

## The change that can break silently: proxy readiness on a strict CNI

!!! warning "Proxy pods can go NotReady on a host-policy-enforcing CNI"
    This is more likely to bite — and bite silently — than the Prometheus break above, because nothing is misconfigured on your side: the policy simply blocks the node.

    The proxy's startup/liveness/readiness probes hit the config API port (8081). The default-on NetworkPolicy admits 8081 only from the controller namespace, but **kubelet probe traffic originates from the node, not a pod namespace**. Most CNIs allow host→pod traffic implicitly, so probes keep working. A CNI that also enforces host policies (Cilium with host policy enforcement, Calico with host endpoints) drops the probes, and **every proxy pod — shared and per-Gateway — goes `NotReady`, taking the data plane down**.

    Two fixes:

    - add an ingress rule admitting the node/kubelet source for port 8081, or
    - set `proxy.networkPolicy.enabled: false` to drop the policy entirely (covers both the shared and per-Gateway planes).

## Verify after upgrade

- **Proxy readiness.** Confirm proxy pods (shared and per-Gateway) reach `Ready` after upgrade. If they stay `NotReady` on a strict CNI, see the warning above — kubelet probes are being dropped by the new NetworkPolicy.
- **Controller → proxy config push.** The controller pushes config to the proxy from its own namespace, which the default policy admits; verify routes still program after upgrade.

## No CRD or values migration

No `GatewayClassConfig` change is required and no values are removed — the defaults flip (`proxy.metrics.enabled`, `proxy.networkPolicy.enabled`) but every key keeps its v3.0 meaning. Pin `proxy.metrics.enabled: false` and/or `proxy.networkPolicy.enabled: false` to retain the exact v3.0 data-plane shape.
