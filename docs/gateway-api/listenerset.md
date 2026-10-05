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
2. The parent Gateway is not refused: a Gateway reporting `Accepted=False` for a reason other than `ListenersNotValid` leaves its ListenerSets `Accepted=False` with `ParentNotAccepted`.
3. The ListenerSet has `Accepted: True` on its status — at least one of its listener entries is conflict-free AND has its TLS cert refs resolved (`ResolvedRefs: True`, or it carries no TLS material).

The Gateway's `status.attachedListenerSets` field is the count of ListenerSets meeting all three criteria, and 0 while the Gateway is refused. When the controller cannot finish that count, for example because a ListenerSet's namespace or a certificate Secret cannot be read, the Gateway keeps the count it had and the controller counts again.

### Hostnames and redirect schemes from a ListenerSet

A route bound through a `ListenerSet` can inherit a hostname and a redirect scheme from its entries only while the parent Gateway exists, can be read, and is not managed by a different Gateway API implementation. A ListenerSet names no GatewayClass of its own, so its parent Gateway's class decides whose it is. If the parent Gateway is absent, or its class names another controller, the entries lend no hostname to a route bound through it and no protocol or port to a scheme-less redirect. If the controller cannot evaluate the ListenerSet or its parent Gateway, the entries lend nothing either, so a route bound through it serves only what its other parents lend until the controller can, or nothing when none lends a hostname; [Routes attached to another implementation's Gateway](limitations.md#routes-attached-to-another-implementations-gateway) lists the causes and the status the route carries meanwhile. A parent whose class is missing or cannot be read at that moment is not treated as another implementation's, so its ListenerSets keep lending hostnames and redirect defaults.

## Precedence and conflict resolution

Per Gateway API spec the effective listener list is concatenated as follows:

1. Listeners declared on the Gateway itself.
2. Listeners from attached ListenerSets, ordered by `metadata.creationTimestamp` (oldest first).
3. Within the same timestamp, ListenerSets are ordered alphabetically by `namespace/name`.

When two listeners share the same `(port, hostname)` tuple, the higher-precedence one wins; the lower-precedence one is marked `Conflicted: true` with reason `HostnameConflict` and `Accepted: false`. When two listeners share a port but disagree on `protocol`, the same precedence applies with reason `ProtocolConflict`. Gateway listeners always win conflicts against ListenerSets. Two of the Gateway's own listeners have no precedence between them, so both are marked `Conflicted: true` and neither serves. A Gateway listener whose protocol this controller does not serve is refused as `UnsupportedProtocol` and takes no part in this: an `HTTP` listener next to a `TCP` one on the same port still serves. A ListenerSet entry with such a protocol still loses to an earlier listener on its port, but claims neither the port's protocol nor its hostname, so a later `HTTP` entry on that port still serves.

A ListenerSet with at least one conflict-free, fully-resolved (`ResolvedRefs: True`) listener still surfaces `Accepted: true` overall, with reason `ListenersNotValid` when another entry is unusable; only the individual conflicting or unresolved entries are rejected. A ListenerSet whose every listener conflicts, has unresolved refs, uses an unservable protocol or has a missing or invalid `allowedRoutes.namespaces.selector` gets `Accepted: false / ListenersNotValid`.

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
| `Accepted` | `True` | `ListenersNotValid` | At least one entry is valid, and another is conflict-marked, has unresolved refs, uses a protocol this controller does not serve or has an `allowedRoutes.namespaces.selector` that is missing or does not parse |
| `Accepted` | `False` | `NotAllowed` | Gateway's `spec.allowedListeners` rejects this ListenerSet. A `selector` that is missing or does not parse rejects every ListenerSet; the message says so without quoting the selector, the controller log names any parse error, and the parent Gateway gets an `InvalidAllowedListeners` Warning Event |
| `Accepted` | `False` | `Pending` | The controller could not evaluate the parent Gateway's `allowedListeners`, for example because the ListenerSet's namespace could not be read for its `selector`, or could not enumerate the sibling ListenerSets. Also set when the controller could not read an entry's cert `Secret` or the `ReferenceGrant`s of its namespace. The controller log names the error and the ListenerSet is reconciled again |
| `Accepted` | `False` | `ParentNotAccepted` | The parent Gateway is refused (`Accepted=False` for a reason other than `ListenersNotValid`), for example because its parameters do not resolve, its namespace is at the data-plane cap, it sets `spec.tls.frontend`, or it requests an unsupported `spec.addresses` type. The message names the parent's reason but not its message, which can mention objects the ListenerSet owner cannot read. Route binding reads only specs, never a status, so a route on such a ListenerSet reports its own verdict for that parent: `NoMatchingParent` when the parent sets `spec.tls.frontend` or requests a `spec.addresses` type other than `Hostname`, `Pending` when the parent's dedicated data plane is refused, and the usual binding result otherwise |
| `Accepted` | `False` | `ListenersNotValid` | No entry is usable: each one is conflict-marked, has unresolved refs, uses a protocol this controller does not serve or has an `allowedRoutes.namespaces.selector` that is missing or does not parse |
| `Programmed` | `True` | `Programmed` | Attached and programmed against the parent Gateway |
| `Programmed` | `False` | `ListenersNotValid` / `NotAllowed` / `ParentNotAccepted` / `Pending` | Mirrors the `Accepted` reason when not programmed |

### Per-entry conditions (`status.listeners[]`)

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Entry accepted |
| `Accepted` | `False` | `HostnameConflict` | Same `(port, hostname)` claimed by a Gateway listener or by an entry of an earlier ListenerSet |
| `Accepted` | `False` | `ProtocolConflict` | Different protocol claimed for the same port |
| `Accepted` | `False` | `UnsupportedValue` | `allowedRoutes.namespaces.from` is `Selector` and the selector is missing or does not parse, so the entry admits no route. The message says the selector is invalid without quoting it; `Programmed` is `False` with reason `Invalid` |
| `Programmed` | `True` | `Programmed` | Entry programmed; routes can bind |
| `Conflicted` | `True` | `HostnameConflict` / `ProtocolConflict` | Conflict surfaced |
| `Conflicted` | `False` | `NoConflicts` | Entry has no conflicts |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Cert refs (if any) resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace cert ref denied by missing ReferenceGrant |
| `ResolvedRefs` | `False` | `InvalidCertificateRef` | Cert Secret missing, wrong type, or missing data |

## AttachedRoutes

Each per-entry status reports `attachedRoutes`: the number of Routes attached to that listener entry and `Accepted` for the ListenerSet. Per the Gateway API spec, attachment depends on the entry's `allowedRoutes` and the Route's `parentRefs`, not on the entry's own status, and only Routes with `Accepted: True` are counted. A Route whose parentRef also matches a usable entry is counted through that same parentRef on a `Conflicted` entry too, and so is a Route on an entry whose `Programmed` is `False` because its TLS certificate ref failed to resolve. A Route whose only matching entries are `Conflicted` is rejected, so it counts nowhere. A Route that is `Accepted: False` with reason `Pending` is not counted either, for example one on a dedicated Gateway whose tunnel claim Cloudflare has not confirmed, so the count can drop while the Cloudflare API is unavailable. Each Route counts once per entry. When the controller cannot finish a count, for example because a Route's namespace cannot be read for a `from: Selector` entry, every entry keeps the count it had, the ListenerSet keeps its `Accepted` condition, and the controller counts again. The field therefore measures binding and blast radius, not whether the entry currently serves traffic. A ListenerSet rejected at the resource level (not permitted by the parent Gateway's `allowedListeners`) reports `attachedRoutes: 0` for every entry, because the entries are not part of any merged Gateway.

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
