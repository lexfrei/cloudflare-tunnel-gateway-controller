# ListenerSet

`ListenerSet` (`gateway.networking.k8s.io/v1`, Standard channel as of v1.5.0) lets a separate resource attach additional listeners to an existing Gateway without modifying the Gateway itself. The typical use case is multi-tenant Gateway management — a platform team owns the Gateway, individual teams own a `ListenerSet` per tenant that contributes hostnames and route-binding rules without ever touching the Gateway spec.

## Quick example

The Gateway opts in to ListenerSet attachment via `spec.allowedListeners`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: shared-gateway
  namespace: platform
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: http
      port: 80
      protocol: HTTP
      hostname: shared.example.com
      allowedRoutes:
        namespaces:
          from: All
  allowedListeners:
    namespaces:
      from: Same
```

A team adds their own listener through a `ListenerSet` in the same namespace:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata:
  name: team-a-listeners
  namespace: platform
spec:
  parentRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: shared-gateway
  listeners:
    - name: team-a-http
      port: 80
      protocol: HTTP
      hostname: team-a.example.com
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              tenant: team-a
```

Routes attach via the ListenerSet (or directly to the Gateway):

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: team-a-app
  namespace: team-a
  labels:
    tenant: team-a
spec:
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: ListenerSet
      name: team-a-listeners
      namespace: platform
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: team-a-app
          port: 8080
```

The route's effective hostname is the parent listener's `team-a.example.com` (inherited because the route declares no `spec.hostnames`).

## How attachment works

A `ListenerSet` is successfully attached to a Gateway when:

1. The parent Gateway's `spec.allowedListeners.namespaces.from` permits the ListenerSet's namespace (`Same`, `All`, `Selector`, or unset/`None` to reject).
2. The ListenerSet has `Accepted: True` on its status — at least one of its listener entries is conflict-free AND has its TLS cert refs resolved (`ResolvedRefs: True`, or it carries no TLS material).

The Gateway's `status.attachedListenerSets` field is the count of ListenerSets meeting both criteria.

## Precedence and conflict resolution

Per Gateway API spec the effective listener list is concatenated as follows:

1. Listeners declared on the Gateway itself.
2. Listeners from attached ListenerSets, ordered by `metadata.creationTimestamp` (oldest first).
3. Within the same timestamp, ListenerSets are ordered alphabetically by `namespace/name`.

When two listeners share the same `(port, hostname)` tuple, the higher-precedence one wins; the lower-precedence one is marked `Conflicted: true` with reason `HostnameConflict` and `Accepted: false`. When two listeners share a port but disagree on `protocol`, the same precedence applies with reason `ProtocolConflict`. Gateway listeners always win conflicts against ListenerSets.

A ListenerSet with at least one conflict-free, fully-resolved (`ResolvedRefs: True`) listener still surfaces `Accepted: true` overall; only the individual conflicting or unresolved entries are rejected. A ListenerSet whose every listener conflicts (or has unresolved refs) gets `Accepted: false / ListenersNotValid`.

## ReferenceGrant scoping

ReferenceGrants applied to a Gateway are **not** inherited by child ListenerSets. A `ListenerSet` referencing a Secret or Service in another namespace needs its own `ReferenceGrant` whose `from.kind` is `ListenerSet`:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: cert-for-listener-set
  namespace: certs
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: ListenerSet
      namespace: platform
  to:
    - group: ""
      kind: Secret
```

## Status conditions

### Top-level ListenerSet conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Permitted by Gateway and at least one entry is valid |
| `Accepted` | `False` | `NotAllowed` | Gateway's `spec.allowedListeners` rejects this ListenerSet |
| `Accepted` | `False` | `ListenersNotValid` | All entries are conflict-marked or have unresolved refs |
| `Programmed` | `True` | `Programmed` | Attached and programmed against the parent Gateway |
| `Programmed` | `False` | `ListenersNotValid` / `NotAllowed` / `Pending` | Mirrors the `Accepted` reason when not programmed |

### Per-entry conditions (`status.listeners[]`)

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Entry accepted |
| `Accepted` | `False` | `HostnameConflict` | Same `(port, hostname)` claimed by a higher-precedence listener |
| `Accepted` | `False` | `ProtocolConflict` | Different protocol claimed for the same port |
| `Programmed` | `True` | `Programmed` | Entry programmed; routes can bind |
| `Conflicted` | `True` | `HostnameConflict` / `ProtocolConflict` | Conflict surfaced |
| `Conflicted` | `False` | `NoConflicts` | Entry has no conflicts |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Cert refs (if any) resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace cert ref denied by missing ReferenceGrant |
| `ResolvedRefs` | `False` | `InvalidCertificateRef` | Cert Secret missing, wrong type, or missing data |

## AttachedRoutes

Each per-entry status reports `attachedRoutes` — the number of Routes bound to that listener entry. Per the Gateway API spec, attachment depends solely on the entry's `allowedRoutes` and the Route's `parentRefs` (plus the Route's own `Accepted` state); the listener's own status does not change the count. A Route attached to an entry that is `Conflicted`, or whose `Programmed` is `False` because its TLS certificate ref failed to resolve, is still counted — the spec requires `attachedRoutes` to be set even when the entry's own `Accepted` condition is `False`. The field therefore measures binding and blast radius, not whether the entry currently serves traffic. A ListenerSet rejected at the resource level (not permitted by the parent Gateway's `allowedListeners`) reports `attachedRoutes: 0` for every entry, because the entries are not part of any merged Gateway.

## DNS automation (external-dns)

If you rely on [external-dns](https://github.com/kubernetes-sigs/external-dns) to publish the tunnel CNAME for your hostnames, note that a route attached **only** via a `ListenerSet` parentRef needs external-dns to follow that parentRef to the parent Gateway's status address. external-dns supports this through the opt-in `--gateway-listener-sets` flag (available since external-dns v0.21.0). Without the flag, external-dns skips `Kind=ListenerSet` parentRefs and the hostname gets no DNS record even though the controller programs the route correctly.

Two ways to handle it:

- **Enable the flag** (recommended): add `--gateway-listener-sets` to the external-dns deployment args. external-dns then resolves the target through the ListenerSet → parent Gateway chain. The `external-dns.kubernetes.io/target` annotation is also honoured directly on `ListenerSet` resources, taking precedence over the parent Gateway's target annotation.
- **Keep a direct Gateway parentRef** alongside the ListenerSet one: the route then has two parents, the controller programs it once, and external-dns resolves the DNS record via the Gateway parent regardless of the flag.

This is an external-dns behaviour, not a controller limitation — the controller programs the route through the tunnel in both cases.

!!! note "Accurate as of late spring 2026"

    The flag name, the minimum external-dns version, and the ListenerSet handling above reflect external-dns as of late spring 2026. external-dns moves fast — before relying on this, confirm against the upstream [Gateway API source docs](https://kubernetes-sigs.github.io/external-dns/latest/docs/sources/gateway-api/), which are the authoritative source.

## Tunnel-specific notes

Cloudflare Tunnel is a single ingress point — `port`, `protocol`, and `tls` on Gateway listeners are accepted for spec compliance but the real TLS termination happens at Cloudflare's edge. The same constraint applies to ListenerSet listeners: per-ListenerSet TLS certificate refs are validated for status (`ResolvedRefs`, including ReferenceGrant for cross-namespace refs), never served — TLS terminates at the Cloudflare edge with Cloudflare's certificates, and parent listener secrets are never readable through a child ListenerSet. Multi-port and protocol-specific behaviour (TCP/UDP) is not supported.

For the full tenant self-service pattern (Selector delegation, hostname-ownership enforcement, collision detection), see the [Multi-Tenancy guide](../guides/multi-tenancy.md).
