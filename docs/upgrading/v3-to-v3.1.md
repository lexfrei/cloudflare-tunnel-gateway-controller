# Upgrading from v3.0 or v3.1 to v3.2

v3.2 hardens multi-tenant isolation. It adds one CRD, `GatewayConfig`, which you apply by hand before upgrading. It is built against Gateway API v1.6 rather than v1.5, and an install that manages RBAC by hand needs new ClusterRole rules; the two sections after the CRD step cover both. Existing `GatewayClassConfig` objects and chart values keep working, and most installs need no values change. Two setups do, and the sections below name the edit: a CNI that enforces host→pod traffic, and an install that already set `proxy.networkPolicy.enabled: true` and relied on its egress restriction. Four behaviours change in ways existing automation can observe. The one that can break a working setup is the data-plane NetworkPolicy, now on by default: on a CNI that also enforces host→pod (kubelet) traffic it takes proxy pod readiness down. The same policy limits the new proxy `/metrics` endpoint to the controller namespace, and an install that already enabled it gets a policy with a different shape. Read the four notes below, then the sections after them.

## Do this first: apply the GatewayConfig CRD

v3.2 adds the namespaced `GatewayConfig` CRD for per-Gateway data planes, and the controller now always runs a reconciler that watches it. Helm installs the chart's `crds/` files only on the first `helm install` and never touches them on upgrade, so an upgraded cluster does not have it. Apply it once before upgrading:

```bash
kubectl apply --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/master/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayconfigs.yaml
```

The `GatewayClassConfig` CRD gains no fields in v3.2 and needs nothing. See [CRD upgrades](index.md#crd-upgrades) for the general rule.

## Move to a Gateway API v1.6 bundle

v3.0 and v3.1 are built against Gateway API v1.5, v3.2 against v1.6. The GatewayClass `SupportedVersion` condition compares the `major.minor` of the installed Gateway API CRD bundle with that version. A v3.2 controller on a v1.5.x bundle keeps running, but reports `SupportedVersion=False` with reason `UnsupportedVersion`. Apply the `standard-install.yaml` asset of a v1.6.x Gateway API release, for example [v1.6.0](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.0), or its `experimental-install.yaml` if you run the experimental channel: applying the standard bundle over experimental CRDs removes the experimental fields from their schema, and the apiserver then prunes those fields. Do it before you switch the controller image, or restart the controller afterwards. The controller does not watch the Gateway API CRDs, so it recomputes `SupportedVersion` only when it next reconciles the GatewayClass. A v3.0 or v3.1 controller runs the same comparison against v1.5, so it reports `UnsupportedVersion` on a v1.6 bundle.

## Manual (non-Helm) installs: update the ClusterRole first

A Helm upgrade brings the new rules with the chart. If you keep RBAC by hand, apply the rules from the v3.2.0 [`deploy/rbac/role.yaml`](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/v3.2.0/deploy/rbac/role.yaml) before you switch the controller image.

Coming from v3.0, the rule v3.1 changed applies too: the controller manages the `gateway-exists` finalizer on GatewayClass, which needs `update` and `patch` on `gatewayclasses`. Without them, every reconcile of a GatewayClass that a Gateway uses fails with `Forbidden` when it tries to add the finalizer, and that class's status conditions stop being updated.

v3.2 adds these rules:

- `gatewayconfigs` (`cf.k8s.lex.la`): `get`, `list`, `watch`.
- `networkpolicies` (`networking.k8s.io`) and `horizontalpodautoscalers` (`autoscaling`): `get`, `list`, `watch`, `create`, `update`, `delete`.
- `services`: `create`, `update`, `delete`, on top of the read verbs it already had.
- `deployments` (`apps`): `create`, `update`, `delete`, on top of `get`, `list`, `watch` and `patch`.
- `secrets`: `create`.

The controller watches `GatewayConfig`, `NetworkPolicy` and `HorizontalPodAutoscaler` objects from startup, whether or not any Gateway opts into a per-Gateway data plane, so the read rules apply to every install. The write verbs are for the objects the controller renders for per-Gateway data planes.

The v3.2 `deploy/controller/deployment.yaml` also gains two settings. The `--proxy-image` flag names the image for per-Gateway proxy Deployments; without it, a Gateway whose `GatewayConfig` sets no `spec.image` is not rendered: it reports `Accepted=False` with reason `InvalidParameters`, and a Warning Event names the missing flag. The `CONTROLLER_NAMESPACE` env var passes the pod's namespace through the downward API; without it the controller reads the namespace from the service account volume, and falls back to `default` when that volume is not mounted.

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

- The new `proxy.metrics.enabled` key defaults to `true`, and with it the proxy serves `/metrics` on the config API port (8081): request-level series (`cftunnel_proxy_*`: in-flight, duration, status classes, bytes, backend errors) and the embedded cloudflared connector metrics. Before v3.2 the proxy served no `/metrics` endpoint at all. The proxy ServiceMonitor stays opt-in (`serviceMonitor.enabled: false` by default).
- `proxy.networkPolicy.enabled` now defaults to `true`. The chart renders an ingress-only NetworkPolicy that admits the config API port (which also carries `/metrics`) only from the controller's own namespace. The proxy data port (8080) takes no in-cluster ingress — tunnel traffic arrives outbound. The controller also renders an equivalent NetworkPolicy for each per-Gateway data plane.

The second change is the one that can break an existing setup: see [proxy readiness](#the-change-that-can-break-silently-proxy-readiness-on-a-strict-cni) below.

## Scraping the new metrics from another namespace

If you scrape the proxy `/metrics` (`serviceMonitor.enabled: true`, or a manual scrape) from a namespace other than the controller's, the default-on NetworkPolicy blocks that scrape, because the policy admits port 8081 only from the controller namespace. A proxy ServiceMonitor enabled before v3.2 already targeted that port, but found no `/metrics` there until this release.

Admit your monitoring namespace:

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

If you do not run a NetworkPolicy-enforcing CNI, the policy is a no-op and scraping is unaffected — but it is then also not providing the isolation, so treat it as defense in depth, not a guarantee. To keep the pre-v3.2 behaviour (no data-plane NetworkPolicy at all), set `proxy.networkPolicy.enabled: false` — that one switch gates BOTH the chart's shared-proxy policy AND the per-Gateway policies the controller renders (it forwards the value as the controller's `--render-network-policy` flag, which deletes any policy it previously rendered).

## The change that can break silently: proxy readiness on a strict CNI

!!! warning "Proxy pods can go NotReady on a host-policy-enforcing CNI"
    This bites silently, because nothing is misconfigured on your side: the policy simply blocks the node.

    The proxy's startup/liveness/readiness probes hit the config API port (8081). The default-on NetworkPolicy admits 8081 only from the controller namespace, but **kubelet probe traffic originates from the node, not a pod namespace**. Most CNIs allow host→pod traffic implicitly, so probes keep working. A CNI that also enforces host policies (Cilium with host policy enforcement, Calico with host endpoints) drops the probes, and **every proxy pod — shared and per-Gateway — goes `NotReady`, taking the data plane down**.

    Two fixes:

    - add an ingress rule admitting the node/kubelet source for port 8081, or
    - set `proxy.networkPolicy.enabled: false` to drop the policy entirely (covers both the shared and per-Gateway planes).

## Verify after upgrade

- **Proxy readiness.** Confirm proxy pods (shared and per-Gateway) reach `Ready` after upgrade. If they stay `NotReady` on a strict CNI, see the warning above — kubelet probes are being dropped by the new NetworkPolicy.
- **Controller → proxy config push.** The controller pushes config to the proxy from its own namespace, which the default policy admits; verify routes still program after upgrade.

## No values removed

No `GatewayClassConfig` change is required and no values are removed. `proxy.networkPolicy.enabled` flips its default to `true`, and the new `proxy.metrics.enabled` key defaults to `true`. Pin `proxy.metrics.enabled: false` and/or `proxy.networkPolicy.enabled: false` to keep the pre-v3.2 metrics and NetworkPolicy behaviour.

## If you already set `proxy.networkPolicy.enabled: true`

The same keys render a different proxy policy in v3.2:

- The policy is ingress-only. Egress restriction moved behind the new `proxy.networkPolicy.egressRestricted` key, which defaults to `false`, so an install that relied on the old egress lock loses it silently. Set `proxy.networkPolicy.egressRestricted: true` to keep it.
- `proxy.networkPolicy.ingress.from` used to be the whole list of admitted sources, and an empty list admitted every source. It is now added on top of the release namespace, which is always admitted.
- The proxy data port (8080) is no longer admitted from inside the cluster. Only the config API port (8081) is.
