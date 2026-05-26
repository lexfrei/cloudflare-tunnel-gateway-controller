# Upgrading

This section documents version-to-version upgrade paths and the breaking changes that go with each release line.

## Available guides

- [v2 → v3](v2-to-v3.md) — the v3 chart collapses to a single L7-proxy data plane, slims the GatewayClassConfig CRD spec, drops the AmneziaWG sidecar, and tightens RBAC.

## Conventions

- Each guide lists the breaking changes and the steps to migrate, in order.
- "No data migration is required for CRs" means existing CRs continue to work; the API server prunes any fields the new schema no longer declares.
- `helm upgrade --reset-then-reuse-values` is preferred over `--reuse-values` when the new chart adds a required value — the latter drops new chart defaults and the install fails on the required check.
