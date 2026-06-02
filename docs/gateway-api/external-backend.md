# ExternalBackend

`ExternalBackend` is a namespaced CRD (`cf.k8s.lex.la/v1alpha1`) that declares an out-of-cluster HTTP(S) origin a route can target as a `backendRef`. Because the v3 data plane is a generic in-process L7 proxy that ultimately just dials a URL, a route can point at an arbitrary external endpoint without a `Service` standing in for it.

## When to use it

Use an `ExternalBackend` when the upstream is not a cluster `Service` and you need an explicit scheme, a host that is not a valid Service name (e.g. an IP literal), or an optional base path:

- A `Service` of type `ExternalName` already routes to a DNS name, but it carries only the hostname and infers the scheme from the port.
- An `ExternalBackend` makes the scheme explicit (`http` / `https`), validates the host/port at admission, and is a first-class, discoverable object you can `kubectl get`.

## Spec

| Field | Required | Description |
| --- | --- | --- |
| `scheme` | Yes | `http` or `https` (enum). |
| `host` | Yes | Hostname or IP (no scheme, port, or path). Bracket IPv6 literals, e.g. `[2001:db8::1]`. |
| `port` | Yes | TCP port, `1`–`65535`. |
| `path` | No | Optional base path prepended when dialing; must begin with `/`. |

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: ExternalBackend
metadata:
  name: payments-api
  namespace: shop
spec:
  scheme: https
  host: api.example.com
  port: 8443
  path: /v1
```

## Referencing it from a route

The `backendRef` names the `ExternalBackend` group/kind. The `backendRef.port` is ignored — `spec.port` always wins:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop
  namespace: shop
spec:
  parentRefs:
    - name: cloudflare-tunnel
  hostnames:
    - shop.example.com
  rules:
    - backendRefs:
        - group: cf.k8s.lex.la
          kind: ExternalBackend
          name: payments-api
```

GRPCRoute references an `ExternalBackend` the same way. A gRPC `ExternalBackend` uses h2c when `scheme: http` and HTTP/2 over TLS (ALPN) when `scheme: https`.

## Cross-namespace references

An `ExternalBackend` in another namespace requires a `ReferenceGrant` in the backend's namespace whose `to` entry names the `ExternalBackend` group/kind — a Service-only grant does not authorize it:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-route-to-external-backend
  namespace: shop
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: storefront
  to:
    - group: cf.k8s.lex.la
      kind: ExternalBackend
```

## Status and failure modes

- A missing `ExternalBackend` surfaces `ResolvedRefs=False, BackendNotFound` on the referencing route and returns HTTP 500 for that backend's traffic fraction (other weighted backends keep serving).
- An unauthorized cross-namespace reference surfaces `ResolvedRefs=False, RefNotPermitted`.

!!! note "No SSRF allowlist"
    The proxy egresses to the operator-authored URL directly. There is no built-in destination allowlist: an `ExternalBackend` is cluster/namespace-authored config, so the trust boundary is namespace write-access plus `ReferenceGrant`, identical to a `Service` of type `ExternalName`.
