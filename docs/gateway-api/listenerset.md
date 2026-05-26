# ListenerSet

`ListenerSet` (`gateway.networking.k8s.io/v1`, Standard channel as of v1.5.1) lets a separate resource attach additional listeners to an existing Gateway without modifying the Gateway itself. The typical use case is multi-tenant Gateway management — a platform team owns the Gateway, individual teams own a `ListenerSet` per tenant that contributes hostnames and route-binding rules without ever touching the Gateway spec.

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
2. The ListenerSet has `Accepted: True` on its status — at least one of its listener entries is conflict-free.

The Gateway's `status.attachedListenerSets` field is the count of ListenerSets meeting both criteria.

## Precedence and conflict resolution

Per Gateway API spec the effective listener list is concatenated as follows:

1. Listeners declared on the Gateway itself.
2. Listeners from attached ListenerSets, ordered by `metadata.creationTimestamp` (oldest first).
3. Within the same timestamp, ListenerSets are ordered alphabetically by `namespace/name`.

When two listeners share the same `(port, hostname)` tuple, the higher-precedence one wins; the lower-precedence one is marked `Conflicted: true` with reason `HostnameConflict` and `Accepted: false`. When two listeners share a port but disagree on `protocol`, the same precedence applies with reason `ProtocolConflict`. Gateway listeners always win conflicts against ListenerSets.

A ListenerSet with at least one conflict-free listener still surfaces `Accepted: true` overall; only the individual conflicting entries are rejected. A ListenerSet whose every listener conflicts gets `Accepted: false / ListenersNotValid`.

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
| `Programmed` | `False` | matches the `Accepted` reason | Not attached |

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

## Tunnel-specific notes

Cloudflare Tunnel is a single ingress point — `port`, `protocol`, and `tls` on Gateway listeners are accepted for spec compliance but the real TLS termination happens at Cloudflare's edge. The same constraint applies to ListenerSet listeners: their TLS material is validated (existence, type, PEM, ReferenceGrant) but not used for cipher negotiation. Multi-port and protocol-specific behaviour (TCP/UDP) is not supported.
