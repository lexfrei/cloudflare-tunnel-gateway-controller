# Knative Serving (net-gateway-api)

This guide covers running [Knative Serving](https://knative.dev/) behind this controller via [`knative-extensions/net-gateway-api`](https://github.com/knative-extensions/net-gateway-api), the known failure mode (revisions stuck not-Ready), why it happens, and the supported workaround.

!!! warning "Revisions get stuck not-Ready with this controller as the only Gateway"
    Wiring Knative's `config-gateway` directly at a Gateway backed by this controller does **not** work today: every revision stays `Ready=Unknown` forever and traffic never shifts off the previous revision. This is a known, tracked limitation ([#511](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/511)), not a config mistake — read on for why, and use the split-horizon workaround below.

## Why revisions stay not-Ready

net-gateway-api gates a KIngress's readiness on two things:

1. `Accepted=True` on the HTTPRoute's parent status. This controller sets it correctly — the HTTPRoute is `Accepted=True` / `ResolvedRefs=True`, and the revision's pod reports `2/2 Running`.
2. An **active HTTP probe dialed directly at the gateway data plane** — the gateway Service endpoints when `config-gateway` sets a `service:`, otherwise the first entry in `Gateway.status.addresses` on port 80/443. The prober expects `200` back with a `K-Network-Hash` response header matching the version it expects.

This controller's data plane is tunnel-only. In tunnel mode the proxy serves all routed traffic exclusively in-process (cloudflared's `OverrideProxy` hook feeds requests straight into the L7 handler) and exposes **no in-cluster HTTP listener** for that traffic. `Gateway.status.addresses` is the tunnel CNAME (`<tunnel-id>.cfargotunnel.com`), which resolves nowhere inside the cluster. The probe can never reach a working listener, so the KIngress sits at `NetworkConfigured=True` / `LoadBalancerReady=Unknown` / `Ready=Unknown` indefinitely, and the ksvc reports `READY=Unknown (Uninitialized)`. Once the previous revision scales to zero, requests to the stuck revision surface as `backend unavailable`.

The net-gateway-api prober logs the failure continuously once this happens:

```text
Probing of http://<ksvc-host>/ failed, IP: <tunnel-id>.cfargotunnel.com:80, ready: false,
error: dial tcp: lookup <tunnel-id>.cfargotunnel.com on 10.96.0.10:53: no such host
```

This is not specific to Cloudflare Tunnel: any Gateway API implementation whose data plane is not directly dialable in-cluster (a pure edge/SaaS proxy, a tunnel, an off-cluster load balancer without a routable in-cluster address) hits the same prober assumption. See [Upstream context](#upstream-context) below.

## Workaround: split-horizon Gateway

net-gateway-api's `config-gateway` ConfigMap (namespace `knative-serving`) accepts **separate** `external-gateways` and `local-gateways` entries, each naming its own `class` / `gateway` / `service`. Point `local-gateways` at a second, cluster-internal Gateway API implementation whose data plane IS directly dialable in-cluster — for example [Cilium's Gateway API support](https://docs.cilium.io/en/stable/network/servicemesh/gateway-api/gateway-api/) — and keep `external-gateways` on the Gateway backed by this controller for real external traffic:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: config-gateway
  namespace: knative-serving
data:
  external-gateways: |
    - class: cloudflare-tunnel
      gateway: cloudflare-tunnel-system/cloudflare-tunnel
  local-gateways: |
    - class: cilium
      gateway: cilium-system/knative-local-gateway
      service: cilium-system/knative-local-gateway
```

With this split:

- The net-gateway-api readiness prober dials the Cilium-backed `local-gateways` entry, which has a real in-cluster Service and pods, so probes succeed and revisions go `Ready=True` normally.
- Public traffic still terminates at the Cloudflare edge and reaches the cluster only through this controller's tunnel — the `external-gateways` entry never needs to be dialable in-cluster because nothing probes it directly.
- Both Gateways can coexist on the same cluster; they only need non-overlapping `class` values and their own GatewayClass objects.

This is a genuine architectural split (two Gateway implementations, two GatewayClasses), not a workaround internal to this controller — it trades one extra in-cluster component for working Knative revision rollouts today.

## What this does not fix

Hard-wiring an in-cluster HTTP listener into this controller's own proxy (so a single Gateway backed by this controller could satisfy both the external tunnel path and net-gateway-api's in-cluster probe) is tracked as the still-open feature half of [#511](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/511). It is not implemented and not documented as a workaround here — the proxy has no code path for it yet. Split-horizon (above) is the only supported approach until that lands.

## Upstream context

- [`knative-extensions/net-gateway-api#979`](https://github.com/knative-extensions/net-gateway-api/issues/979) — the prober's coupling to a directly-dialable data-plane topology, filed against this exact failure mode.
- [`knative/serving#5129`](https://github.com/knative/serving/issues/5129) — a related failure of the ingress status prober's topology assumptions.
- [`knative-extensions/net-gateway-api#665`](https://github.com/knative-extensions/net-gateway-api/issues/665) — the `service:`-less fallback (probing `Gateway.status.addresses` directly), which is the exact path that fails for a tunnel CNAME.

## Checklist

| Step | Mechanism |
| --- | --- |
| Diagnose "stuck not-Ready" | Check net-gateway-api controller logs for `Probing of http://... failed` against a `cfargotunnel.com` address |
| Fix external traffic | Keep this controller's Gateway in `config-gateway`'s `external-gateways` |
| Fix revision readiness | Add a second, in-cluster-dialable Gateway (e.g. Cilium) under `local-gateways` |
| Do not attempt | Hand-rolling an in-cluster listener for this controller's proxy — not implemented yet ([#511](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/511)) |
