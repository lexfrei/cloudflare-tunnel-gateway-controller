# Upgrading

This section documents version-to-version upgrade paths and the breaking changes that go with each release line.

## Available guides

- [v2 → v3](v2-to-v3.md) — the v3 chart collapses to a single L7-proxy data plane, slims the GatewayClassConfig CRD spec, drops the AmneziaWG sidecar, and tightens RBAC.
- [v3.0 → v3.1](v3-to-v3.1.md) — multi-tenant isolation hardening: data-plane metrics and config-API NetworkPolicy default on, a new `RouteShadowed` condition/Event, and a longer proxy drain window. No CRD or values migration.
- [v3.4 → v3.5](v3.4-to-v3.5.md) — changes that can break a working setup, some needing a values edit. `proxy.networkPolicy.enabled` defaults to `true`, so a scraper sharing the release namespace loses the proxy's `/metrics` after the upgrade even on an install that never set the key; and a values file with `networkPolicy.enabled: true` and `networkPolicy.cloudflareIpRanges` emptied now [stops the render](v3.4-to-v3.5.md#change-that-can-break-an-emptied-cloudflareipranges-stops-the-render). The `GatewayClassConfig` CRD also needs a one-time re-apply (see [CRD upgrades](#crd-upgrades)).

## Conventions

- Each guide lists the breaking changes and the steps to migrate, in order.
- "No data migration is required for CRs" means existing CRs continue to work; the API server prunes any fields the new schema no longer declares.
- `helm upgrade --reset-then-reuse-values` is preferred over `--reuse-values` when the new chart adds a required value — the latter drops new chart defaults and the install fails on the required check.

## CRD upgrades

Helm installs the files under the chart's `crds/` directory only on the FIRST `helm install`; `helm upgrade` deliberately never touches them. Apply the CRDs once after upgrading to a chart version that adds a CRD (for example `GatewayConfig` for per-Gateway data planes) **or that adds a field to an existing one**.

A field the installed CRD does not declare is pruned by the apiserver on write, with no error and no Event: it disappears on the way in and reads back unset, so a setting you made is not in force and nothing says so.

```bash
kubectl apply \
  --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/master/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayclassconfigs.yaml \
  --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/master/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayconfigs.yaml \
  --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/master/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_externalbackends.yaml
```
