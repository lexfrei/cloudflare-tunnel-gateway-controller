# Upgrading

This section documents version-to-version upgrade paths and the breaking changes that go with each release line.

## Available guides

- [v2 → v3](v2-to-v3.md) — the v3 chart collapses to a single L7-proxy data plane, slims the GatewayClassConfig CRD spec, drops the AmneziaWG sidecar, and tightens RBAC.
- [v3.0 → v3.1](v3-to-v3.1.md) — multi-tenant isolation hardening: data-plane metrics and config-API NetworkPolicy default on, a new `RouteShadowed` condition/Event, and a longer proxy drain window. No CRD or values migration.

## Conventions

- Each guide lists the breaking changes and the steps to migrate, in order.
- "No data migration is required for CRs" means existing CRs continue to work; the API server prunes any fields the new schema no longer declares.
- `helm upgrade --reset-then-reuse-values` is preferred over `--reuse-values` when the new chart adds a required value — the latter drops new chart defaults and the install fails on the required check.

## CRD upgrades

Helm installs the files under the chart's `crds/` directory only on the FIRST `helm install`; `helm upgrade` deliberately never touches them. After upgrading to a chart version that adds a CRD (for example `GatewayConfig` for per-Gateway data planes), apply the new CRDs once:

```bash
kubectl apply --filename https://raw.githubusercontent.com/lexfrei/cloudflare-tunnel-gateway-controller/master/charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayconfigs.yaml
```
