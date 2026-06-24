# Knative Serving Integration

This guide explains how to run this controller as the Gateway API implementation
behind [Knative Serving](https://knative.dev/docs/serving/) via
[`knative-extensions/net-gateway-api`](https://github.com/knative-extensions/net-gateway-api).

## Why a special setup is needed

Knative Serving gates a Revision's readiness on its `Ingress`
(`networking.internal.knative.dev/Ingress`, "KIngress") resource becoming
`Ready=True`. With `net-gateway-api`, that requires **two** things:

1. The `HTTPRoute` parents report `Accepted=True` / `ResolvedRefs=True`. ✅
   This controller already sets those correctly.
2. An **active HTTP probe** that `net-gateway-api` dials *directly at the gateway
   data plane* from inside the cluster. It expects `HTTP 200` carrying a
   `K-Network-Hash` header that matches the version Knative expects. ❌

The blocker is #2. This controller's data plane is **tunnel-only**: the L7 proxy
serves traffic in-process through `cloudflared`'s edge transport, and exposes no
in-cluster HTTP listener for routed traffic. `Gateway.status.addresses` is the
tunnel CNAME (`<tunnel-id>.cfargotunnel.com`), which is **not resolvable or
routable from inside the cluster**. So `net-gateway-api`'s prober logs:

```text
Probing of http://<ksvc-host>/ failed, IP: <tunnel-id>.cfargotunnel.com:80,
ready: false, error: dial tcp: lookup <tunnel-id>.cfargotunnel.com ...: no such host
```

and the KIngress stays `LoadBalancerReady=Unknown` / `Ready=Unknown` forever —
the Revision never goes Ready, and a new Revision never rolls over.

## The fix: opt-in in-cluster listener

The chart provides an opt-in `proxy.inClusterListener` that makes the data plane
probeable in-cluster without changing the default (tunnel-only) behaviour for
non-Knative users. Enabling it does three things:

1. **Starts an extra HTTP listener** on the proxy port (`8080`) serving the
   *same* L7 handler `cloudflared` serves, so an in-cluster request is routed to
   the real backend (the Knative queue-proxy / activator), which answers the
   probe with `200` + `K-Network-Hash`.
2. **Renders a dedicated `ClusterIP` Service** whose **first** port is the proxy
   port and is named `http` — `net-gateway-api` dials a Service's first port
   (falling back from a name match against `http`/`http2`/`http-80`), so this
   makes the probe reach the listener deterministically.
3. **Admits the prober's namespace** through the proxy `NetworkPolicy` (the
   shipped policy otherwise allows only the config-API port from the controller
   namespace).

## Setup

### 1. Install the controller with the listener enabled

```yaml
proxy:
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
  inClusterListener:
    enabled: true
    # The namespace where net-gateway-api's controller (the probe source) runs.
    # The release namespace is always admitted in addition, so a co-located
    # install works without setting this. Standard Knative installs put Serving
    # and net-gateway-api together in `knative-serving` (the default).
    networkPolicy:
      namespaces:
        - knative-serving
```

This renders a Service named `<release>-proxy-incluster` (in the chart namespace)
exposing the proxy port `8080` as `http`. Note that exact name down — you will
point `net-gateway-api` at it in step 3.

```bash
kubectl -n cloudflare-tunnel-system get svc \
  -l app.kubernetes.io/component=proxy
```

### 2. Create the Gateway Knative will use

Create a `Gateway` (and `GatewayClass` if not using the chart-managed one) that
`net-gateway-api` reconciles against. The `GatewayClass` must match the
`controllerName` of this controller instance.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: knative-gateway
  namespace: cloudflare-tunnel-system
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: http
      protocol: HTTP
      port: 8080
```

!!! note
    The Gateway's `status.addresses` will carry the tunnel CNAME (used by
    external DNS / external-dns). That is expected and correct for edge traffic;
    the in-cluster probe uses the Service from step 1 instead, via
    `config-gateway` below.

### 3. Point net-gateway-api's `config-gateway` at the Service

Patch `net-gateway-api`'s `config-gateway` ConfigMap (in the Knative install
namespace) so each gateway entry sets `service:` to the in-cluster Service from
step 1, `gateway:` to the Gateway from step 2, and `class:` to this controller's
GatewayClass name:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: config-gateway
  namespace: knative-serving
data:
  external-gateways: |
    - gateway: cloudflare-tunnel-system/knative-gateway
      service: cloudflare-tunnel-system/<release>-proxy-incluster
      class: cloudflare-tunnel
  local-gateways: |
    - gateway: cloudflare-tunnel-system/knative-gateway
      service: cloudflare-tunnel-system/<release>-proxy-incluster
      class: cloudflare-tunnel
```

With `service:` set, `net-gateway-api` dials the Service's endpoints (the proxy
pods) on the `http` port instead of the unresolvable tunnel CNAME. The probe now
returns `200` + `K-Network-Hash`, the KIngress goes `Ready=True`, and publishing
a new Revision rolls over cleanly.

## Verifying

```bash
# Probe should now succeed (200) against the proxy pods on :8080
kubectl -n knative-serving logs -l app=net-gateway-api -c controller \
  | grep Probing

# KIngress should flip to Ready=True
kubectl get ingress -A -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'

# Revision should go Ready
kubectl get revisions
```

## Supported features & caveats

- **Weighted traffic splitting works.** The earlier "traffic splitting not
  supported" log is a known false negative (separate issue) — the in-process L7
  proxy honours `HTTPRoute` backend weights, so Knative's canary/rollout traffic
  splits function end-to-end.
- **The in-cluster listener is HTTP (plaintext) on the cluster network.** It is
  scoped by the proxy `NetworkPolicy` to the release namespace plus the
  namespaces you list in `proxy.inClusterListener.networkPolicy.namespaces`.
  Keep that list to only the net-gateway-api controller's namespace.
- **Off by default.** Non-Knative deployments are unaffected: the tunnel-only
  data plane is unchanged unless `proxy.inClusterListener.enabled` is `true`.
