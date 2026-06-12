# Multi-Tenancy

This guide describes how to run multiple tenants (teams, namespaces) behind one controller installation, what each isolation layer guarantees, and where the boundaries are.

## The isolation model at a glance

| Threat | Fail-fast layer | Authoritative layer |
| --- | --- | --- |
| A tenant claims another tenant's hostname | `ValidatingAdmissionPolicy` rejects the write at admission | The controller refuses to program the route into the proxy and the tunnel |
| A route is silently shadowed by a higher-precedence route | `cf.k8s.lex.la/RouteShadowed` condition + Warning Event | Hostname ownership makes cross-tenant shadowing impossible |
| Tenant listeners escape operator control | `allowedListeners` / `allowedRoutes` admission status | The same filters are enforced in the data path (merge view) |
| Tenants share one process and one tunnel | Listener scoping (this page) | A dedicated proxy and tunnel per Gateway — see [Per-Gateway Isolation](per-gateway-isolation.md) |

Every protection ships as two independent layers by design: if one layer is bypassed (an older cluster without `ValidatingAdmissionPolicy`, a deleted policy, a write path admission does not gate), the other still holds.

## The hostname-capture problem

The Gateway API defines no route-to-route hostname ownership. When one listener with no hostname pin allows routes from all namespaces, any namespace can create an `HTTPRoute` claiming any hostname — including one another team already serves. Route precedence (oldest `creationTimestamp` wins, then alphabetical `{namespace}/{name}`) then decides who receives the traffic, silently.

The spec-aligned answer is scoping listeners per tenant; the sections below layer enforcement on top.

## Canonical pattern: per-tenant listeners

Pin each tenant to its hostname suffix with a dedicated listener and restrict which namespaces may bind to it:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: shared-gateway
  namespace: cloudflare-tunnel-system
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: team-a
      port: 443
      protocol: HTTPS
      hostname: "*.team-a.example.com"
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              tenant: team-a
    - name: team-b
      port: 443
      protocol: HTTPS
      hostname: "*.team-b.example.com"
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              tenant: team-b
```

The controller enforces listener-to-route hostname intersection, so a route in a `tenant: team-a` namespace cannot serve `app.team-b.example.com` through the `team-a` listener. Avoid the permissive combination — a single unpinned listener with `allowedRoutes.namespaces.from: All` — unless every namespace is equally trusted.

## Tenant self-service with ListenerSet

Instead of the platform team editing the Gateway for every tenant, delegate listener management via [ListenerSet](../gateway-api/listenerset.md): the Gateway opts in with `allowedListeners.namespaces.from: Selector`, and each tenant owns a `ListenerSet` (its hostnames, its own TLS certificate references) in its namespace. Conflicting listeners are rejected with `Conflicted=True` following GEP-1713 precedence (Gateway listeners first, then oldest `creationTimestamp`, then `{namespace}/{name}`).

Note the TLS boundary: per-ListenerSet TLS certificate references are validated for status (`ResolvedRefs`, including ReferenceGrant for cross-namespace refs) but never served — TLS terminates at the Cloudflare edge with Cloudflare's certificates. Parent listener secrets are never readable through a child ListenerSet.

## Enforcing hostname ownership

The `hostnameOwnershipPolicy` Helm value binds each namespace to ONE allowed hostname suffix via a namespace label and enforces it twice:

```yaml
# values.yaml
hostnameOwnershipPolicy:
  enabled: true
  labelKey: cf.k8s.lex.la/hostname-suffix
  namespaceSelector:
    matchLabels:
      tenancy: enforced
```

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: team-a
  labels:
    tenancy: enforced
    cf.k8s.lex.la/hostname-suffix: team-a.example.com
```

With this in place:

1. **Admission layer** (Kubernetes 1.30+): a `ValidatingAdmissionPolicy` denies `HTTPRoute`/`GRPCRoute` writes whose hostnames fall outside the namespace suffix. On older clusters set `hostnameOwnershipPolicy.admissionPolicy: false` to skip this layer.
2. **Controller layer**: independent of admission, the controller rejects violating routes at binding time (`Accepted=False`, reason `HostnameNotPermitted`) and never programs them into the proxy config or the Cloudflare ingress document.

Both layers are fail-closed within the policed scope:

- A policed namespace without the suffix label cannot program any route.
- A route without explicit `spec.hostnames` is rejected — an empty list would inherit the listener hostname, which is exactly the capture vector.
- `evil<suffix>` does not pass: a hostname must equal the suffix or be a subdomain of it (`.`-boundary).

Model constraints: label values cannot contain `*` or `,` and cap at 63 characters (hostnames go to 253 — a longer suffix is not expressible as a label value), so each namespace gets exactly one suffix; uppercase in the label value is normalized. An empty `namespaceSelector` polices every namespace — fail-closed everywhere, including system namespaces — so scope it deliberately (a marker label as above, or `matchExpressions` excluding `kube-system` and the controller namespace).

!!! warning "Scope difference between the two layers"
    The admission policy matches **every** `HTTPRoute`/`GRPCRoute` written in a policed namespace, regardless of which Gateway implementation the route targets — admission cannot resolve parentRefs. The controller layer polices only routes binding to THIS controller's Gateways. On a cluster running multiple Gateway API implementations (Istio, Envoy Gateway, …), enabling the admission layer will also constrain routes meant for those implementations; scope `namespaceSelector` to namespaces that only use this controller, or run with `admissionPolicy: false` and rely on the controller layer alone.

## Detecting collisions

Same-hostname routes merge legally per the Gateway API, so a collision is not an error — but it should never be invisible. When a route's `(hostname, match)` pair is exactly claimed by a higher-precedence route, the losing route carries a dedicated condition (its `Accepted` stays `True`):

```text
$ kubectl get httproute capture-attempt -o yaml
...
  conditions:
    - type: cf.k8s.lex.la/RouteShadowed
      status: "True"
      reason: HostnameMatchShadowed
      message: 'rule 0 match (host "app.team-a.example.com", ...) is shadowed by
        HTTPRoute team-a/app rule 0 (older creationTimestamp); ...'
```

A Warning Event with reason `RouteShadowed` mirrors the condition for `kubectl events` and event-driven alerting. The condition clears automatically when the collision is resolved.

## The shared-plane boundary

Everything above is admission- and control-plane-level isolation: all tenants still share one proxy process and one Cloudflare Tunnel. That means shared fate (a crash or overload affects everyone), a shared edge identity, and no per-tenant traffic accounting at the tunnel level.

For hard isolation, opt a Gateway into a dedicated data plane — its own proxy Deployment and its own tunnel — via `Gateway.spec.infrastructure.parametersRef`. See [Per-Gateway Isolation](per-gateway-isolation.md).

## Per-tenant observability

The proxy exposes request-level Prometheus metrics labelled by matched hostname pattern (bounded cardinality — never the raw client Host), so per-tenant request rates, latencies, and error classes are visible on the shared plane too. See [Metrics & Alerting](../operations/metrics.md).

## Checklist

| Step | Mechanism |
| --- | --- |
| Pin listeners per tenant | `listener.hostname` + `allowedRoutes: Selector` |
| Delegate listener self-service | `allowedListeners: Selector` + tenant `ListenerSet` |
| Enforce hostname ownership twice | `hostnameOwnershipPolicy.enabled: true` + namespace labels |
| Watch for collisions | `cf.k8s.lex.la/RouteShadowed` condition / `RouteShadowed` Events |
| Hard-isolate critical tenants | [Per-Gateway data plane](per-gateway-isolation.md) |
